package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// copySwapFixture is one database with a schema of its own for a test that
// drives the copy-and-swap route through the front door's fold.
type copySwapFixture struct {
	cfg    dbconn.Config
	pool   *pgxpool.Pool
	schema string
}

func newCopySwapFixture(t *testing.T) copySwapFixture {
	t.Helper()
	cfg := dbconn.Config{URL: testutil.StartPostgres(t)}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return copySwapFixture{cfg: cfg, pool: pool, schema: testutil.NewSchema(t, pool)}
}

// exec runs sql with every {schema} substituted by the fixture's schema.
func (f copySwapFixture) exec(t *testing.T, sql string) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), f.inSchema(sql))
	require.NoError(t, err)
}

// inSchema substitutes every {schema} in sql with the fixture's schema.
func (f copySwapFixture) inSchema(sql string) string {
	return strings.ReplaceAll(sql, "{schema}", f.schema)
}

// createOrders is the table every test here changes: a bigserial key, a
// few thousand rows, and a secondary index the swap has to carry.
func (f copySwapFixture) createOrders(t *testing.T) {
	t.Helper()
	f.exec(t, `
		CREATE TABLE {schema}.orders (
			id   bigserial PRIMARY KEY,
			qty  integer NOT NULL,
			note text,
			sku  text
		)`)
	f.exec(t, `CREATE INDEX orders_qty_idx ON {schema}.orders (qty)`)
	f.exec(t, `
		INSERT INTO {schema}.orders (qty, note, sku)
		SELECT g, 'note ' || g, 'sku' || (g % 7)
		FROM generate_series(1, 2500) g`)
}

// options is a copy-and-swap policy over the fixture's database with room
// on disk, quiesced for the orders table, the shape every test here starts
// from.
func (f copySwapFixture) options() Options {
	opts := DefaultOptions()
	opts.CopySwap = CopySwapOptions{
		Connection:    f.cfg,
		FreeDiskBytes: testutil.UnlimitedDisk,
		BlockWrites:   f.schema + ".orders",
	}
	return opts
}

// run drives the route for alter, with every {schema} substituted.
func (f copySwapFixture) run(t *testing.T, alter string, opts Options) (verdict.Verdict, error) {
	t.Helper()
	st, err := statement.ParseOne(f.inSchema(alter))
	require.NoError(t, err)
	return copyAndSwap(t.Context(), f.pool, st, opts, slog.New(slog.DiscardHandler))
}

func (f copySwapFixture) hasColumn(t *testing.T, table, column string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT EXISTS (
			SELECT 1 FROM pg_attribute a
			JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1 AND c.relname = $2 AND a.attname = $3 AND NOT a.attisdropped)`,
		f.schema, table, column).Scan(&exists))
	return exists
}

// columnType is the declared type of table's column as information_schema
// reports it, the shape a retype through the front door changes.
func (f copySwapFixture) columnType(t *testing.T, table, column string) string {
	t.Helper()
	var typ string
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT data_type FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2 AND column_name = $3`,
		f.schema, table, column).Scan(&typ))
	return typ
}

func (f copySwapFixture) relationExists(t *testing.T, table string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1 AND c.relname = $2)`, f.schema, table).Scan(&exists))
	return exists
}

func (f copySwapFixture) rowCount(t *testing.T, table string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM `+pgx.Identifier{f.schema, table}.Sanitize()).Scan(&n))
	return n
}

// A quiesced table carried from no row to done through the front door's
// fold: the statement's shape is live under the source's name with every
// row the source had, the retained source is gone, and the one verdict
// says so — the copy-and-swap outcome, a nil error, and a detail that
// reports what the verification pass covered.
func TestCopyAndSwapRunsAQuiescedTableToOneVerdict(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)

	v, err := f.run(t, `ALTER TABLE {schema}.orders DROP COLUMN note`, f.options())
	require.NoError(t, err)

	assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome)
	assert.Equal(t, f.schema+".orders", v.Table)
	assert.Empty(t, v.Reason)
	assert.Empty(t, v.Code)
	assert.Contains(t, v.Detail, "verified 2500 rows")
	assert.Contains(t, v.Detail, "the retained source is dropped")

	assert.False(t, f.hasColumn(t, "orders", "note"), "the gated statement's shape is live")
	assert.EqualValues(t, 2500, f.rowCount(t, "orders"))
	assert.False(t, f.relationExists(t, schemachange.OldName(f.schema, "orders")), "the retained source is dropped")
}

// KeepOld leaves the retained source under its _old name, and the verdict's
// detail says it is kept rather than dropped, so the operator reading it
// knows a rollback window is open.
func TestCopyAndSwapKeepsTheOldTableWhenAsked(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	opts := f.options()
	opts.CopySwap.KeepOld = true

	v, err := f.run(t, `ALTER TABLE {schema}.orders DROP COLUMN note`, opts)
	require.NoError(t, err)

	assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome)
	assert.Contains(t, v.Detail, "kept under its _old name")
	assert.True(t, f.relationExists(t, schemachange.OldName(f.schema, "orders")), "the retained source is kept")
	assert.True(t, f.hasColumn(t, schemachange.OldName(f.schema, "orders"), "note"), "the retained source has the source's shape")
}

// A table shape the route does not carry refuses before anything is built:
// an UNLOGGED table is a capability boundary with the preflight's own cause
// on the verdict, under the one copy-and-swap reason, and the verdict's
// JSON carries reason, class, and cause for an importer to route on.
func TestCopyAndSwapRefusesAnUnsupportedShapeWithItsCause(t *testing.T) {
	f := newCopySwapFixture(t)
	f.exec(t, `CREATE UNLOGGED TABLE {schema}.scratch (id bigserial PRIMARY KEY, note text)`)

	v, err := f.run(t, `ALTER TABLE {schema}.scratch DROP COLUMN note`, f.options())
	require.NoError(t, err, "a refusal is a verdict, not an error")

	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonCopyAndSwapRefused, v.Reason)
	assert.Equal(t, verdict.ClassCapabilityBoundary, v.Class)
	assert.Equal(t, verdict.Cause(preflight.CopySwapCauseUnlogged), v.Cause)
	assert.True(t, f.hasColumn(t, "scratch", "note"), "nothing was executed")

	var wire struct {
		Outcome string `json:"outcome"`
		Reason  string `json:"reason"`
		Class   string `json:"class"`
		Cause   string `json:"cause"`
	}
	encoded, err := v.JSON()
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(encoded), &wire))
	assert.Equal(t, "refused", wire.Outcome)
	assert.Equal(t, "copy-and-swap-refused", wire.Reason)
	assert.Equal(t, "capability-boundary", wire.Class)
	assert.Equal(t, "copy-and-swap-unlogged", wire.Cause)
}

// The environment the operator describes is checked before the shadow is
// built: no free space on the volume is an environmental refusal with the
// disk-headroom cause, and the table is untouched.
func TestCopyAndSwapRefusesWithoutDiskHeadroom(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	opts := f.options()
	opts.CopySwap.FreeDiskBytes = 0

	v, err := f.run(t, `ALTER TABLE {schema}.orders DROP COLUMN note`, opts)
	require.NoError(t, err)

	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonCopyAndSwapRefused, v.Reason)
	assert.Equal(t, verdict.ClassEnvironmental, v.Class)
	assert.Equal(t, verdict.Cause(preflight.CopySwapCauseDiskHeadroom), v.Cause)
	assert.True(t, f.hasColumn(t, "orders", "note"), "nothing was executed")
	assert.False(t, f.relationExists(t, schemachange.ShadowName(f.schema, "orders")), "no shadow was built")
}

// progressLog is an OnCheckpoint hook that records every row it is shown,
// from whichever goroutine the save ran on, and stops the run at the first
// save in stopAt when that is set.
type progressLog struct {
	stopAt checkpoint.Phase
	mu     sync.Mutex
	rows   []checkpoint.Checkpoint
}

// errHostStopped is what the hook stops a run with.
var errHostStopped = errors.New("the host is draining")

func (l *progressLog) hook(_ context.Context, cp checkpoint.Checkpoint) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rows = append(l.rows, cp)
	if cp.Phase == l.stopAt {
		return errHostStopped
	}
	return nil
}

// phases is the sequence of phases the hook was shown, with consecutive
// repeats collapsed: the copy saves its watermark in copying as often as
// the interval fires, and how many times depends on the clock.
func (l *progressLog) phases() []checkpoint.Phase {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []checkpoint.Phase
	for _, cp := range l.rows {
		if len(out) == 0 || out[len(out)-1] != cp.Phase {
			out = append(out, cp.Phase)
		}
	}
	return out
}

// lastIn returns the last row the hook was shown in phase.
func (l *progressLog) lastIn(t *testing.T, phase checkpoint.Phase) checkpoint.Checkpoint {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, row := range slices.Backward(l.rows) {
		if row.Phase == phase {
			return row
		}
	}
	require.FailNowf(t, "phase never saved", "the hook saw no save in %s", phase)
	return checkpoint.Checkpoint{}
}

// inspectRow reads the table's checkpoint row the way a host would between
// two runs, without the run's lock.
func (f copySwapFixture) inspectRow(t *testing.T) checkpoint.Checkpoint {
	t.Helper()
	store, err := checkpoint.NewStore(f.pool, checkpoint.Options{})
	require.NoError(t, err)
	cp, err := store.Inspect(t.Context(), f.schema, "orders")
	require.NoError(t, err)
	return cp
}

// The seam's progress hook is shown every save of the row, in the order
// the run makes them, so a host that records what it is shown has the
// run's whole course: copying from the fresh row through the complete
// watermark, then verifying, cutover, and done, each naming the table.
func TestCopyAndSwapReportsEverySaveToTheHost(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	progress := &progressLog{}
	opts := f.options()
	opts.CopySwap.OnCheckpoint = progress.hook

	v, err := f.run(t, `ALTER TABLE {schema}.orders DROP COLUMN note`, opts)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome)

	assert.Equal(t, []checkpoint.Phase{
		checkpoint.PhaseCopying,
		checkpoint.PhaseVerifying,
		checkpoint.PhaseCutover,
		checkpoint.PhaseDone,
	}, progress.phases())
	for _, cp := range progress.rows {
		assert.Equal(t, f.schema, cp.Schema)
		assert.Equal(t, "orders", cp.Table)
	}
	assert.False(t, progress.rows[0].Watermark.Valid(), "the fresh row has nothing landed")
	assert.True(t, progress.lastIn(t, checkpoint.PhaseCopying).Watermark.Complete(),
		"the copy's last save in copying carries the complete watermark")
}

// A host that returns an error from the hook stops the run at that save:
// the verdict is a failure whose detail says the same statement resumes
// it, the row stands at the phase the host stopped in, and the next run
// picks up from there — saving only the phases that remained — to the same
// executed verdict a run that was never stopped reports.
func TestCopyAndSwapStopsWhereTheHostSaysAndResumes(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	const alter = `ALTER TABLE {schema}.orders DROP COLUMN note`
	stop := &progressLog{stopAt: checkpoint.PhaseVerifying}
	opts := f.options()
	opts.CopySwap.OnCheckpoint = stop.hook

	v, err := f.run(t, alter, opts)
	require.ErrorIs(t, err, errHostStopped)
	assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
	assert.Equal(t, string(executor.CodeExecutionFailed), v.Code)
	assert.Contains(t, v.Detail, "resumes from the row")
	assert.True(t, v.WritesBlocked, "the stopped run left its fence on the source")
	assert.Equal(t, checkpoint.PhaseVerifying, f.inspectRow(t).Phase, "the row stands where the host stopped")
	assert.True(t, f.hasColumn(t, "orders", "note"), "the source is untouched while the run is stopped")

	resumed := &progressLog{}
	opts.CopySwap.OnCheckpoint = resumed.hook
	v, err = f.run(t, alter, opts)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome)
	assert.Equal(t, []checkpoint.Phase{checkpoint.PhaseCutover, checkpoint.PhaseDone}, resumed.phases(),
		"the resumed run saves only the phases the stopped run had left")
	assert.False(t, f.hasColumn(t, "orders", "note"))
	assert.EqualValues(t, 2500, f.rowCount(t, "orders"))
}

// An option set the Runner refuses is a caller's wiring error, reported
// before the preflight reads anything: no verdict, the Runner's own error
// to match on, and the table untouched. The volume reports no free space,
// which the preflight would refuse as disk-headroom, so the wiring error
// coming back instead shows the preflight never ran.
func TestCopyAndSwapRefusesANegativeIntervalBeforeReading(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	opts := f.options()
	opts.CopySwap.CheckpointInterval = -time.Second
	opts.CopySwap.FreeDiskBytes = 0

	v, err := f.run(t, `ALTER TABLE {schema}.orders DROP COLUMN note`, opts)
	require.ErrorIs(t, err, schemachange.ErrInvalidOptions)
	assert.Zero(t, v)
	assert.True(t, f.hasColumn(t, "orders", "note"))
	assert.False(t, f.relationExists(t, schemachange.ShadowName(f.schema, "orders")), "no shadow was built")
}

// Copier options the copier would refuse — here a negative worker count —
// are the same kind of wiring error, refused before the shadow is built
// and the source fenced, rather than once the copy starts with the source
// refusing its writes until the value is corrected (ST-6).
func TestCopyAndSwapRefusesInvalidCopierOptionsBeforeFencingTheSource(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	opts := f.options()
	opts.CopySwap.Copier = copier.Options{Workers: -1}

	v, err := f.run(t, `ALTER TABLE {schema}.orders DROP COLUMN note`, opts)
	require.ErrorIs(t, err, schemachange.ErrInvalidOptions)
	assert.ErrorIs(t, err, copier.ErrInvalidOptions)
	assert.Zero(t, v)
	assert.False(t, f.relationExists(t, schemachange.ShadowName(f.schema, "orders")), "no shadow was built")
	f.exec(t, `
		INSERT INTO {schema}.orders (qty, note)
		VALUES (1, 'written after the refusal')`)
	assert.EqualValues(t, 2501, f.rowCount(t, "orders"), "the source still takes writes")
}

// A host that stops the run at the done save has stopped nothing: the
// swap has committed and the row is done, so the verdict is the executed
// one with a nil error, not a failure whose rerun would start the change
// again on a table that already has it.
func TestCopyAndSwapReportsAnExecutedChangeWhenTheHostStopsAtTheDoneSave(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	stop := &progressLog{stopAt: checkpoint.PhaseDone}
	opts := f.options()
	opts.CopySwap.OnCheckpoint = stop.hook

	v, err := f.run(t, `ALTER TABLE {schema}.orders DROP COLUMN note`, opts)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome)
	assert.Empty(t, v.Code)
	assert.Equal(t, checkpoint.PhaseDone, stop.lastIn(t, checkpoint.PhaseDone).Phase, "the host was shown the done row")
	assert.Equal(t, checkpoint.PhaseDone, f.inspectRow(t).Phase)
	assert.False(t, f.hasColumn(t, "orders", "note"), "the change is complete")
	assert.EqualValues(t, 2500, f.rowCount(t, "orders"))
}

// midCopy reports whether cp is a save the copy made while it was still
// running: in copying, with a landed watermark short of the key space.
// The fresh row has nothing landed and the copy's last save carries the
// complete watermark, so neither is one.
func midCopy(cp checkpoint.Checkpoint) bool {
	if cp.Phase != checkpoint.PhaseCopying {
		return false
	}
	return cp.Watermark.Valid() && !cp.Watermark.Complete()
}

// A copy that outlasts the checkpoint interval saves its landed watermark
// while it runs, and the host is shown those saves from the copy's saving
// goroutine. The copy is sized to one worker and ten-row chunks, so its
// 250 chunks outlast a one-millisecond interval many times over. A hook
// error on such a save stops the copy there: the row stands at the
// watermark the host was shown, and the next run resumes from it to the
// executed verdict with every row.
func TestCopyAndSwapStopsAtAPeriodicSaveOfTheCopy(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	const alter = `ALTER TABLE {schema}.orders DROP COLUMN note`
	var (
		mu      sync.Mutex
		stopped checkpoint.Checkpoint
	)
	opts := f.options()
	opts.CopySwap.CheckpointInterval = time.Millisecond
	opts.CopySwap.Copier = copier.Options{
		Workers: 1,
		Chunker: copier.ChunkerOptions{InitialRows: 10, MinRows: 10, MaxRows: 10},
	}
	opts.CopySwap.OnCheckpoint = func(_ context.Context, cp checkpoint.Checkpoint) error {
		if !midCopy(cp) {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		stopped = cp
		return errHostStopped
	}

	v, err := f.run(t, alter, opts)
	require.ErrorIs(t, err, errHostStopped, "a save landed while the copy was still running")
	assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
	mu.Lock()
	shown := stopped
	mu.Unlock()
	row := f.inspectRow(t)
	assert.Equal(t, checkpoint.PhaseCopying, row.Phase)
	assert.Equal(t, shown.Watermark, row.Watermark, "the row stands at the save the host stopped at")
	assert.True(t, f.hasColumn(t, "orders", "note"), "the source is untouched while the run is stopped")

	opts.CopySwap.OnCheckpoint = nil
	v, err = f.run(t, alter, opts)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome)
	assert.False(t, f.hasColumn(t, "orders", "note"))
	assert.EqualValues(t, 2500, f.rowCount(t, "orders"))
}
