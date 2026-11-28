package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// The tests here drive `pg-sprite discard SCHEMA.TABLE`: abandoning a
// stopped copy-and-swap run without running its change. Each stops a real
// run of
//
//	ALTER TABLE orders ALTER COLUMN qty TYPE bigint
//
// through the migrate front door, at a checkpoint save where a crash or a
// Ctrl-C would leave it, then discards it through the command and checks
// the outcome against the catalog: pg_class for the shadow,
// pg_replication_slots and pg_publication for a live run's hold, a write
// for an offline run's fence, and the checkpoint table for the row.

// discardRetype routes to copy-and-swap: int to bigint rewrites every row.
const discardRetype = `ALTER TABLE %s.orders ALTER COLUMN qty TYPE bigint`

// errStopRun is what a stopped run's checkpoint hook returns: the run
// stops there, its row as that save left it, as after a crash.
var errStopRun = errors.New("stop the run at this checkpoint")

// discardFixture is the orders table with its rows, on a server that
// decodes WAL for a live run's fixture.
type discardFixture struct {
	url    string
	pool   *pgxpool.Pool
	schema string
}

// newDiscardFixture creates the orders table with a thousand rows on the
// server at url.
func newDiscardFixture(t *testing.T, url string) discardFixture {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	f := discardFixture{url: url, pool: pool, schema: testutil.NewSchema(t, pool)}
	f.exec(t, `
		CREATE TABLE %s.orders (
			id  int PRIMARY KEY,
			qty int NOT NULL
		)`)
	f.exec(t, `
		INSERT INTO %s.orders (id, qty)
		SELECT g, g * 10
		FROM generate_series(1, 1000) g`)
	return f
}

// exec runs sql with %s standing for the fixture's schema.
func (f discardFixture) exec(t *testing.T, sql string) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), fmt.Sprintf(sql, pgx.Identifier{f.schema}.Sanitize()))
	require.NoError(t, err)
}

// table is the fixture table in the spelling discard and --restart take.
func (f discardFixture) table() string { return f.schema + ".orders" }

// migrateCmd is the copy-and-swap run of discardRetype the fixture stops,
// offline when blockWrites is set.
func (f discardFixture) migrateCmd(blockWrites bool) *MigrateCmd {
	cmd := newMigrateCmd(f.url, fmt.Sprintf(discardRetype, f.schema))
	cmd.CopyAndSwap = true
	cmd.FreeDisk = byteSize(testutil.UnlimitedDisk)
	if blockWrites {
		cmd.BlockWrites = f.table()
	}
	return cmd
}

// runRetype runs the change with the CLI's options and hook as the
// checkpoint hook, through the same front door the migrate command calls.
func (f discardFixture) runRetype(t *testing.T, blockWrites bool, hook func(context.Context, checkpoint.Checkpoint) error) error {
	t.Helper()
	cmd := f.migrateCmd(blockWrites)
	opts := cmd.options(slog.New(slog.DiscardHandler))
	opts.CopySwap.OnCheckpoint = hook
	pool, err := dbconn.NewPool(t.Context(), cmd.Config())
	require.NoError(t, err)
	defer pool.Close()
	st, err := statement.ParseOne(cmd.Alter)
	require.NoError(t, err)
	_, err = migrate.Run(t.Context(), pool, st, opts)
	return err
}

// stopRetypeAt runs the change and stops it at its first save in phase,
// returning the row the stopped run left.
func (f discardFixture) stopRetypeAt(t *testing.T, phase checkpoint.Phase, blockWrites bool) checkpoint.Checkpoint {
	t.Helper()
	err := f.runRetype(t, blockWrites, func(_ context.Context, cp checkpoint.Checkpoint) error {
		if cp.Phase == phase {
			return errStopRun
		}
		return nil
	})
	require.ErrorIs(t, err, errStopRun)
	row, found := f.row(t)
	require.True(t, found, "the stopped run left its row")
	require.Equal(t, phase, row.Phase)
	return row
}

// row reads the table's checkpoint row, reporting whether there is one.
func (f discardFixture) row(t *testing.T) (checkpoint.Checkpoint, bool) {
	t.Helper()
	store, err := checkpoint.NewStore(f.pool, checkpoint.Options{})
	require.NoError(t, err)
	row, err := store.Inspect(t.Context(), f.schema, "orders")
	if errors.Is(err, checkpoint.ErrNotFound) || errors.Is(err, checkpoint.ErrTableMissing) {
		return checkpoint.Checkpoint{}, false
	}
	require.NoError(t, err)
	return row, true
}

// relationPresent reports whether the fixture's schema has a relation of
// the name.
func (f discardFixture) relationPresent(t *testing.T, name string) bool {
	t.Helper()
	var present bool
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT EXISTS (
			SELECT 1
			FROM pg_catalog.pg_class c
			JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1
			  AND c.relname = $2
		)`, f.schema, name).Scan(&present))
	return present
}

// insertOrder writes one row to the source, returning the server's error.
func (f discardFixture) insertOrder(t *testing.T, id int) error {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), fmt.Sprintf(`
		INSERT INTO %s.orders (id, qty)
		VALUES ($1, 1)`, pgx.Identifier{f.schema}.Sanitize()), id)
	return err
}

// qtyType is the type of orders.qty: integer before the change, bigint
// once its swap committed.
func (f discardFixture) qtyType(t *testing.T) string {
	t.Helper()
	var typ string
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT data_type
		FROM information_schema.columns
		WHERE table_schema = $1
		  AND table_name = 'orders'
		  AND column_name = 'qty'`, f.schema).Scan(&typ))
	return typ
}

// assertSourceUntouched checks that orders still has the shape and the
// rows it had before the run: the change was not run.
func (f discardFixture) assertSourceUntouched(t *testing.T, rows int) {
	t.Helper()
	assert.Equal(t, "integer", f.qtyType(t), "the change was not run")
	var count int
	require.NoError(t, f.pool.QueryRow(t.Context(), fmt.Sprintf(`
		SELECT count(*)
		FROM %s.orders`, pgx.Identifier{f.schema}.Sanitize())).Scan(&count))
	assert.Equal(t, rows, count, "every row of the source stands")
}

// runDiscard runs the discard command with --json and returns its report
// and error.
func runDiscard(t *testing.T, url, table string) (discardReport, error) {
	t.Helper()
	cmd := &DiscardCmd{
		DBFlags: DBFlags{
			URL:              url,
			LockTimeout:      3 * time.Second,
			StatementTimeout: 30 * time.Second,
		},
		Table: table,
		JSON:  true,
	}
	var out strings.Builder
	err := cmd.run(t.Context(), &out)
	var report discardReport
	require.NoError(t, json.Unmarshal([]byte(out.String()), &report), "stdout: %s", out.String())
	return report, err
}

// A live run stopped while it catches up holds its shadow, its replication
// slot and publication, and its row. Discard drops all four and names
// each; the source keeps its shape and every row, since the change is not
// run.
func TestDiscardDropsAStoppedLiveRun(t *testing.T) {
	f := newDiscardFixture(t, testutil.StartPostgresWithSettings(t, "wal_level=logical", "wal_sender_timeout=2s"))
	stopped := f.stopRetypeAt(t, checkpoint.PhaseCatchingUp, false)
	require.NotEmpty(t, stopped.SlotName, "a live run names its slot")
	require.True(t, slotPresent(t, f.pool, stopped.SlotName))
	require.True(t, publicationPresent(t, f.pool, stopped.PublicationName))
	require.True(t, f.relationPresent(t, stopped.ShadowTable))

	report, err := runDiscard(t, f.url, f.table())
	require.NoError(t, err)
	assert.Equal(t, discardReport{
		FormatVersion: discardFormatVersion,
		Table:         f.table(),
		Outcome:       discardOutcomeDiscarded,
		RowPhase:      "catching_up",
		Shadow:        stopped.ShadowTable,
		Slot:          stopped.SlotName,
		Publication:   stopped.PublicationName,
	}, report)
	assert.False(t, slotPresent(t, f.pool, stopped.SlotName), "the slot no longer holds WAL")
	assert.False(t, publicationPresent(t, f.pool, stopped.PublicationName))
	assert.False(t, f.relationPresent(t, stopped.ShadowTable))
	_, found := f.row(t)
	assert.False(t, found, "the checkpoint row is deleted")
	f.assertSourceUntouched(t, 1000)
}

// An offline run stopped while it verifies leaves the source fenced, so
// every write fails with SQLSTATE 55006. Discard takes the fence off with
// the shadow and the row, and writes succeed again.
func TestDiscardDropsAStoppedOfflineRunAndItsWriteFence(t *testing.T) {
	f := newDiscardFixture(t, testutil.StartPostgres(t))
	stopped := f.stopRetypeAt(t, checkpoint.PhaseVerifying, true)
	var fenced *pgconn.PgError
	require.ErrorAs(t, f.insertOrder(t, 1001), &fenced, "the stopped run's fence refuses writes")
	require.Equal(t, "55006", fenced.Code)

	report, err := runDiscard(t, f.url, f.table())
	require.NoError(t, err)
	assert.Equal(t, discardReport{
		FormatVersion: discardFormatVersion,
		Table:         f.table(),
		Outcome:       discardOutcomeDiscarded,
		RowPhase:      "verifying",
		Shadow:        stopped.ShadowTable,
		WriteFence:    true,
	}, report)
	require.NoError(t, f.insertOrder(t, 1001), "the source takes writes again")
	assert.False(t, f.relationPresent(t, stopped.ShadowTable))
	_, found := f.row(t)
	assert.False(t, found, "the checkpoint row is deleted")
	f.assertSourceUntouched(t, 1001)
}

// A run in progress holds the table's lock for as long as it runs; the
// test holds it the same way, over a stopped offline run's leftovers.
// Discard is refused at once with exit 2 and drops nothing: the shadow,
// the fence, and the row all stand.
func TestDiscardRefusesWhileARunHoldsTheTableLock(t *testing.T) {
	f := newDiscardFixture(t, testutil.StartPostgres(t))
	stopped := f.stopRetypeAt(t, checkpoint.PhaseVerifying, true)
	holder, err := dbconn.AcquireTableLock(t.Context(), dbconn.Config{URL: f.url}, f.schema, "orders")
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, holder.Release(context.WithoutCancel(t.Context()))) })

	report, err := runDiscard(t, f.url, f.table())
	require.ErrorIs(t, err, verdict.ErrRefused, "a refusal exits with the refusal code")
	assert.Equal(t, discardOutcomeRefused, report.Outcome)
	assert.Equal(t, discardCauseTableLockHeld, report.Cause)
	assert.Empty(t, report.RowPhase)
	assert.Empty(t, report.Shadow)
	assert.False(t, report.WriteFence)

	assert.True(t, f.relationPresent(t, stopped.ShadowTable), "the shadow stands")
	row, found := f.row(t)
	require.True(t, found, "the row stands")
	assert.Equal(t, checkpoint.PhaseVerifying, row.Phase)
	var fenced *pgconn.PgError
	require.ErrorAs(t, f.insertOrder(t, 1001), &fenced, "the fence stands")
	assert.Equal(t, "55006", fenced.Code)
}

// A table no run has touched has nothing to discard: the command says so
// and exits 0.
func TestDiscardWithNoStoppedRunReportsNothing(t *testing.T) {
	f := newDiscardFixture(t, testutil.StartPostgres(t))

	report, err := runDiscard(t, f.url, f.table())
	require.NoError(t, err, "nothing to discard is an answer, not an error")
	assert.Equal(t, discardReport{
		FormatVersion: discardFormatVersion,
		Table:         f.table(),
		Outcome:       discardOutcomeNothing,
	}, report)
	f.assertSourceUntouched(t, 1000)
}

// A run that stopped after its swap committed has already put the change
// live: the source's name is borne by the new table. That is past the
// point of discarding, so discard is refused with restart-swap-committed
// and exit 2, and the cutover row stands for a rerun of the statement to
// finish. The run is stopped there by holding its checkpoint row locked
// from the cutover save, so the done save after the swap times out.
func TestDiscardRefusesARunWhoseSwapCommitted(t *testing.T) {
	f := newDiscardFixture(t, testutil.StartPostgres(t))
	var holder pgx.Tx
	err := f.runRetype(t, true, func(ctx context.Context, cp checkpoint.Checkpoint) error {
		if cp.Phase != checkpoint.PhaseCutover {
			return nil
		}
		tx, err := f.pool.Begin(context.WithoutCancel(ctx))
		if err != nil {
			return err
		}
		holder = tx
		_, err = tx.Exec(ctx, `
			SELECT 1
			FROM pgsprite.pgsprite_checkpoint
			FOR UPDATE`)
		return err
	})
	require.Error(t, err, "the done save timed out on the locked row")
	require.NotNil(t, holder)
	require.NoError(t, holder.Rollback(t.Context()))
	require.Equal(t, "bigint", f.qtyType(t), "the swap committed: the table under the source's name has the new type")

	report, err := runDiscard(t, f.url, f.table())
	require.ErrorIs(t, err, verdict.ErrRefused)
	assert.Equal(t, discardOutcomeRefused, report.Outcome)
	assert.Equal(t, string(schemachange.CauseRestartSwapCommitted), report.Cause)
	row, found := f.row(t)
	require.True(t, found)
	assert.Equal(t, checkpoint.PhaseCutover, row.Phase, "the row stands for the rerun that finishes the change")
	assert.Equal(t, "bigint", f.qtyType(t), "the swapped-in table stands")
}

// A name no table bears fails with exit 1, not a refusal, and prints no
// report, as a --restart naming the wrong table does.
func TestDiscardOfATableThatDoesNotExistFails(t *testing.T) {
	f := newDiscardFixture(t, testutil.StartPostgres(t))
	cmd := &DiscardCmd{DBFlags: DBFlags{URL: f.url}, Table: f.schema + ".nonesuch"}
	var out strings.Builder
	err := cmd.run(t.Context(), &out)
	assert.ErrorIs(t, err, preflight.ErrTableNotFound)
	assert.NotErrorIs(t, err, verdict.ErrRefused)
	assert.Empty(t, out.String())
}
