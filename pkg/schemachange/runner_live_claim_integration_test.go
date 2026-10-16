package schemachange_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/statement"
)

// The tests here cover how a live run claims its slot and what it does
// when the slot's name is taken or the capture it opened on the slot
// ends: the lock on the slot's name (ST-3), a slot the run did not make,
// the connections a claim holds, and a stream that dies under the fill.

// claimWait bounds a run's wait on the slot's lock and its finish once the
// lock is let go.
const claimWait = 30 * time.Second

// adminShutdown is the SQLSTATE a backend reports to its client when
// pg_terminate_backend ends it.
const adminShutdown = "57P01"

// A run claiming its slot waits for the engine's lock on the slot's name
// (ST-3): while another holder has it no slot exists, and the run goes on
// to done once it is let go.
func TestRunnerWaitsForTheSlotLockBeforeMakingItsSlot(t *testing.T) {
	f := newDecodingFixture(t, 100)
	name := f.target.DecodingName()
	runner, err := schemachange.NewRunner(f.cfg, f.store, liveRunOptions(schemachange.RunOptions{}))
	require.NoError(t, err)
	done := make(chan error, 1)
	var wg sync.WaitGroup
	t.Cleanup(wg.Wait)

	var waited, slotMade bool
	err = f.store.WithSlotLock(t.Context(), f.cfg, name, func(ctx context.Context) error {
		wg.Go(func() {
			_, err := runner.Run(t.Context(), f.pool, f.target, f.liveStatement(t))
			done <- err
		})
		waited = assert.Eventually(t, func() bool {
			var waiting bool
			err := f.pool.QueryRow(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM pg_catalog.pg_stat_activity
					WHERE datname = pg_catalog.current_database()
					  AND wait_event_type = 'Lock' AND wait_event = 'advisory')`).Scan(&waiting)
			return err == nil && waiting
		}, claimWait, 20*time.Millisecond, "the run waits for the slot's lock")
		_, slotMade, err = decode.InspectSlot(ctx, f.pool, name)
		return err
	})
	require.NoError(t, err)

	assert.True(t, waited, "the run waited for the slot's lock")
	assert.False(t, slotMade, "no slot is made while another holder has the lock")
	select {
	case err := <-done:
		require.NoError(t, err, "the run finishes once the lock is let go")
	case <-time.After(claimWait):
		t.Fatal("the run did not finish once the lock was let go")
	}
}

// An orphan slot of the derived name with no row is reported as it is: the
// run that met it leaves it, with its publication, for the reaper or the
// operator, since only what a run made is its to drop (ST-3).
func TestFreshRunLeavesAnOrphanSlotItMet(t *testing.T) {
	f := newDecodingFixture(t, 100)
	slot, err := decode.CreateSlot(t.Context(), f.cfg, f.pool, f.target)
	require.NoError(t, err)
	require.NoError(t, slot.Close(t.Context()))

	_, err = f.runLive(t, schemachange.RunOptions{})

	var exists *decode.SlotExistsError
	require.ErrorAs(t, err, &exists)
	_, found, err := decode.InspectSlot(t.Context(), f.pool, f.target.DecodingName())
	require.NoError(t, err)
	assert.True(t, found, "the slot the run did not make is still there")
	var published bool
	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_publication WHERE pubname = $1)`, f.target.DecodingName()).Scan(&published))
	assert.True(t, published, "the publication the run did not make is still there")
	_, err = f.store.Inspect(t.Context(), f.table.Schema, f.table.Table)
	assert.ErrorIs(t, err, checkpoint.ErrNotFound, "no row records a run that never claimed its slot")
}

// As many live tables as the pool has connections, started together
// through one pool, all reach done: a claim on a slot holds no pool
// connection the other claims need, so none waits on the others.
func TestRunnersOnOnePoolDoNotStarveEachOther(t *testing.T) {
	const tables = 4
	f := newDecodingFixture(t, 100)
	cfg := f.cfg
	cfg.MaxConns = tables
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	store, err := checkpoint.NewStore(pool, checkpoint.Options{})
	require.NoError(t, err)
	type job struct {
		target preflight.CopySwapTarget
		st     statement.Statement
	}
	var jobs []job
	for range tables {
		tbl := testutil.NewWorkloadTable(t, f.pool)
		require.NoError(t, tbl.SeedRows(t.Context(), 100))
		target, err := preflight.CheckCopySwap(t.Context(), f.pool, tbl.Schema, tbl.Table,
			preflight.CopySwapEnvironment{LogicalDecoding: true, FreeDiskBytes: testutil.UnlimitedDisk})
		require.NoError(t, err)
		st, err := statement.ParseOne(fmt.Sprintf(liveAlter, tbl.Qualified()))
		require.NoError(t, err)
		jobs = append(jobs, job{target: target, st: st})
	}
	ctx, cancel := context.WithTimeout(t.Context(), claimWait)
	defer cancel()

	errs := make([]error, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Go(func() {
			runner, err := schemachange.NewRunner(cfg, store, liveRunOptions(schemachange.RunOptions{}))
			if err != nil {
				errs[i] = err
				return
			}
			_, errs[i] = runner.Run(ctx, pool, j.target, j.st)
		})
	}
	wg.Wait()

	for i, err := range errs {
		assert.NoError(t, err, "table %d reaches done on a pool shared with %d other claims", i, tables-1)
	}
}

// A capture whose stream ends under the run stops the fill it runs beside
// and fails the run with the stream's own failure — the server ending the
// walsender — not with the cancellation the failure caused and not with a
// wait on a marker the dead stream can never confirm.
func TestRunnerReportsACaptureThatFailed(t *testing.T) {
	f := newDecodingFixture(t, 2500)
	var endOnce sync.Once
	endStream := func(ctx context.Context, cp checkpoint.Checkpoint) error {
		if cp.Phase != checkpoint.PhaseCatchingUp {
			return nil
		}
		var err error
		endOnce.Do(func() {
			_, err = f.pool.Exec(ctx, `
				SELECT pg_catalog.pg_terminate_backend(active_pid)
				FROM pg_catalog.pg_replication_slots
				WHERE slot_name = $1 AND active_pid IS NOT NULL`, f.target.DecodingName())
		})
		return err
	}
	runner, err := schemachange.NewRunner(f.cfg, f.store, liveRunOptions(schemachange.RunOptions{AfterCheckpoint: endStream}))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), claimWait)
	defer cancel()

	_, err = runner.Run(ctx, f.pool, f.target, f.liveStatement(t))

	require.Error(t, err)
	assert.NotErrorIs(t, err, context.DeadlineExceeded, "the run stops when its capture fails instead of waiting on a stream that is gone")
	assert.NotErrorIs(t, err, context.Canceled, "the fill's cancellation is the capture's doing, not the failure reported")
	var pgErr *pgconn.PgError
	if assert.True(t, errors.As(err, &pgErr), "the error is the stream's own failure: %v", err) {
		assert.Equal(t, adminShutdown, pgErr.Code, "the walsender was terminated")
	}
}
