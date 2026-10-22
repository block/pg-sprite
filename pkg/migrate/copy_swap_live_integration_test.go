package migrate_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/verdict"
)

// The tests here drive the front door's live copy-and-swap path — slot,
// stream, catch-up, swap — over a table a concurrent workload writes the
// whole time, the way an embedding caller or the CLI runs it on a table
// the application never stops using. Two oracles judge the result (TM-5):
// the retained source and the live table hold the same rows, and the live
// table's catalog record equals one the same statement produced natively
// on an identical table.

// liveFixture is a server that decodes WAL, a pool over one of its
// databases, and the workload table the run changes.
type liveFixture struct {
	url   string
	pool  *pgxpool.Pool
	table testutil.WorkloadTable
}

// liveSeededRows is how many rows each live table starts with: enough for
// several chunks and a live pass that outlasts the poll that stops the
// load, few enough that the whole run stays well inside the test's budget.
const liveSeededRows = 4000

// newLiveFixture starts a server with logical decoding and seeds a
// workload table of shape in a database of its own.
func newLiveFixture(t *testing.T, shape testutil.WorkloadShape) liveFixture {
	t.Helper()
	serverURL := testutil.StartPostgresWithSettings(t, "wal_level=logical", "wal_sender_timeout=2s")
	url := testutil.NewDatabase(t, serverURL)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url, MaxConns: 16})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	table := testutil.NewWorkloadTableOf(t, pool, shape)
	require.NoError(t, table.SeedRows(t.Context(), liveSeededRows))
	return liveFixture{url: url, pool: pool, table: table}
}

// options is the live copy-and-swap policy over the fixture's database:
// not quiesced, so the run takes a slot and streams, and keeping the old
// table so the row oracle has the retained source to read.
func (f liveFixture) options() migrate.Options {
	opts := runOptions()
	opts.CopySwap = migrate.CopySwapOptions{
		Connection:    dbconn.Config{URL: f.url},
		FreeDiskBytes: testutil.UnlimitedDisk,
		KeepOld:       true,
	}
	return opts
}

// liveLoad is the workload every live run here carries: inserts, updates
// on a hot set and at random, and deletes, with half the updates rewriting
// the out-of-line column — each a change the stream and the drain carry
// differently. Unique-key swaps and key moves are left out: a move that
// straddles a chunk the copier is reading collides on the copier's own
// insert with the shadow row the flush already landed (CO-4's open
// window), so a load that moves keys runs only once the copy has landed.
var liveLoad = testutil.LoadSpec{
	Seed: 1, Workers: 2, RatePerSecond: 40,
	Mix:                  testutil.Mix{Insert: 2, Update: 3, Delete: 1},
	HotRowFraction:       0.2,
	ToastRewriteFraction: 0.5,
}

// loadPollInterval paces the checkpoint poll that stops the load.
const loadPollInterval = 10 * time.Millisecond

// stopLoadAtVerifying stops load once the run's checkpoint row says
// verifying or later and returns the phase it stopped at, or zero when the
// run returned before the poll saw one. The writer runs through the copy
// and the catch-up and stops as the live pass begins, the same bound the
// Runner's own live tests set: the changes it made that the stream has yet
// to deliver are the pass's to settle, while a write landing after the
// swap committed belongs to the live table alone, which the old-versus-live
// oracle would count as a difference rather than as the behaviour it is.
// The poll reads the row through the checkpoint store's inspection, the
// same read a status command makes, so the test needs no hook into the run.
func (f liveFixture) stopLoadAtVerifying(t *testing.T, load *testutil.LoadGenerator) (stop func(), stoppedAt func() checkpoint.Phase) {
	t.Helper()
	store, err := checkpoint.NewStore(f.pool, checkpoint.Options{})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		phase checkpoint.Phase
	)
	wg.Go(func() {
		ticker := time.NewTicker(loadPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			cp, err := store.Inspect(ctx, f.table.Schema, f.table.Table)
			if errors.Is(err, checkpoint.ErrNotFound) || errors.Is(err, checkpoint.ErrTableMissing) || errors.Is(err, context.Canceled) {
				continue
			}
			if err != nil {
				t.Errorf("inspect the run's checkpoint row: %v", err)
				return
			}
			if !pastCatchUp(cp.Phase) {
				continue
			}
			summary, err := load.Stop()
			mu.Lock()
			phase = cp.Phase
			mu.Unlock()
			assert.NoError(t, err, "the load stopped cleanly; summary %+v", summary)
			return
		}
	})
	stop = func() { cancel(); wg.Wait() }
	stoppedAt = func() checkpoint.Phase {
		mu.Lock()
		defer mu.Unlock()
		return phase
	}
	return stop, stoppedAt
}

// pastCatchUp reports whether phase is one the run reaches only once the
// catch-up has drained everything the copy did not see.
func pastCatchUp(phase checkpoint.Phase) bool {
	switch phase {
	case checkpoint.PhaseVerifying, checkpoint.PhaseCutover, checkpoint.PhaseDone:
		return true
	default:
		return false
	}
}

// runUnderLoad carries the fixture's table through alter with liveLoad
// writing it until the run's live pass begins, and returns the verdict
// and what the load did.
func (f liveFixture) runUnderLoad(t *testing.T, alter string) (verdict.Verdict, testutil.Summary) {
	t.Helper()
	load := testutil.StartLoad(t, f.pool, f.table, liveLoad)
	stopPolling, stoppedAt := f.stopLoadAtVerifying(t, load)
	v, err := migrate.Run(t.Context(), f.pool, parseOne(t, fmt.Sprintf(alter, f.table.Qualified())), f.options())
	stopPolling()
	require.NoError(t, err)
	require.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome, "%s: %s", v.Reason, v.Detail)
	require.NotZero(t, stoppedAt(), "the run returned before the poll saw the verifying save, so the load wrote across the swap and the oracle cannot judge the result")

	summary, err := load.Stop()
	require.NoError(t, err)
	require.Positive(t, summary.Inserts, "the load inserted while the run copied")
	require.Positive(t, summary.Updates, "the load updated while the run copied")
	require.Positive(t, summary.Deletes, "the load deleted while the run copied")
	return v, summary
}

// assertZeroDiff holds the swapped-in table to both oracles against the
// reference: a fresh workload table of shape that alter changed natively.
// Row oracle: the retained source and the live table hold the same rows,
// compared by value under the live table's types. Catalog oracle: the live
// table's catalog record equals the reference's, so the retype, its
// identity sequence, its indexes, and its constraints are what the native
// statement would have produced.
func (f liveFixture) assertZeroDiff(t *testing.T, shape testutil.WorkloadShape, alter string) {
	t.Helper()
	testutil.AssertConverged(t, f.pool,
		testutil.RelationRef{Schema: f.table.Schema, Table: schemachange.OldName(f.table.Schema, f.table.Table)},
		testutil.RelationRef{Schema: f.table.Schema, Table: f.table.Table},
		testutil.ConvergeOptions{})

	reference := testutil.NewWorkloadTableOf(t, f.pool, shape)
	_, err := f.pool.Exec(t.Context(), fmt.Sprintf(alter, reference.Qualified()))
	require.NoError(t, err)
	want, err := testutil.ReadTableCatalog(t.Context(), f.pool, reference.Schema, reference.Table)
	require.NoError(t, err)
	got, err := testutil.ReadTableCatalog(t.Context(), f.pool, f.table.Schema, f.table.Table)
	require.NoError(t, err)
	assert.Equal(t, want, got, "the swapped-in table differs from one the statement changed natively")
}

// The flagship change: the identity primary key itself widens from integer
// to bigint while the table is written. Every chunk the copier reads, every
// change the stream carries, and every key the drain pairs is under the
// retyped column; the live table holds the source's rows under the new
// type with the identity sequence retyped alongside, as the native
// statement does.
func TestRunCopySwapRetypesAnIdentityKeyUnderLoad(t *testing.T) {
	shape := testutil.WorkloadShape{IDType: "integer"}
	f := newLiveFixture(t, shape)
	const alter = `ALTER TABLE %s ALTER COLUMN id TYPE bigint`

	v, _ := f.runUnderLoad(t, alter)
	assert.Equal(t, f.table.Schema+".workload", v.Table)
	f.assertZeroDiff(t, shape, alter)
}

// A numeric column widens in precision and scale while the table is
// written. Widening precision alone is a relabel the planner routes
// natively; a scale change rewrites every value, so it is the numeric
// widening that takes the copy-and-swap path, with every stored and
// streamed amount converted under the new scale.
func TestRunCopySwapWidensANumericColumnUnderLoad(t *testing.T) {
	f := newLiveFixture(t, testutil.WorkloadShape{})
	const alter = `ALTER TABLE %s ALTER COLUMN amount TYPE numeric(14,4)`

	v, _ := f.runUnderLoad(t, alter)
	assert.Equal(t, f.table.Schema+".workload", v.Table)
	f.assertZeroDiff(t, testutil.WorkloadShape{}, alter)
}
