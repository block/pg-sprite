package migrate

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
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

// options is a quiesced copy-and-swap policy over the fixture's database
// with room on disk, the shape every test here starts from.
func (f copySwapFixture) options() Options {
	opts := DefaultOptions()
	opts.CopySwap = CopySwapOptions{
		Connection:    f.cfg,
		FreeDiskBytes: testutil.UnlimitedDisk,
		Quiesced:      true,
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
