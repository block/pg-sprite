package migrate_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/suggest"
	"github.com/block/pg-sprite/pkg/verdict"
)

// destructiveOptions is runOptions with the destructive acknowledgement
// naming table, for a test whose subject is something other than the
// destructive-change gate but whose statement drops something.
func destructiveOptions(table string) migrate.Options {
	opts := runOptions()
	opts.AllowDestructive = table
	return opts
}

// destructiveFixture is one database with a schema of its own holding the
// orders table every destructive-change test drops something from.
type destructiveFixture struct {
	cfg    dbconn.Config
	pool   *pgxpool.Pool
	schema string
}

// newDestructiveFixture creates the orders table: a column, a CHECK
// constraint, and a NOT NULL for each destructive operation kind to
// discard, and rows whose values a drop would lose.
func newDestructiveFixture(t *testing.T) destructiveFixture {
	t.Helper()
	cfg := dbconn.Config{URL: testutil.StartPostgres(t)}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	f := destructiveFixture{cfg: cfg, pool: pool, schema: testutil.NewSchema(t, pool)}
	f.exec(t, `
		CREATE TABLE {schema}.orders (
			id   bigint PRIMARY KEY,
			qty  integer NOT NULL CONSTRAINT orders_qty_positive CHECK (qty > 0),
			note text NOT NULL
		)`)
	f.exec(t, `CREATE INDEX orders_qty_idx ON {schema}.orders (qty)`)
	f.exec(t, `
		INSERT INTO {schema}.orders (id, qty, note)
		SELECT g, g, 'note ' || g
		FROM generate_series(1, 500) g`)
	return f
}

// inSchema substitutes every {schema} in sql with the fixture's schema.
func (f destructiveFixture) inSchema(sql string) string {
	return strings.ReplaceAll(sql, "{schema}", f.schema)
}

func (f destructiveFixture) exec(t *testing.T, sql string) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), f.inSchema(sql))
	require.NoError(t, err)
}

// run drives the imperative front door for sql, every {schema} substituted.
func (f destructiveFixture) run(t *testing.T, sql string, opts migrate.Options) (verdict.Verdict, error) {
	t.Helper()
	return migrate.Run(t.Context(), f.pool, parseOne(t, f.inSchema(sql)), opts)
}

// table is the acknowledgement that names the fixture's orders table.
func (f destructiveFixture) table() string { return f.schema + ".orders" }

// copySwapOptions is a copy-and-swap policy that blocks the orders table's
// writes, over the fixture's database with room on disk, so a statement
// routed to that backend can run.
func (f destructiveFixture) copySwapOptions() migrate.Options {
	opts := runOptions()
	opts.CopySwap = migrate.CopySwapOptions{
		Connection:    f.cfg,
		FreeDiskBytes: testutil.UnlimitedDisk,
		BlockWrites:   f.table(),
	}
	return opts
}

func (f destructiveFixture) hasColumn(t *testing.T, column string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT EXISTS (
			SELECT 1 FROM pg_attribute
			WHERE attrelid = ($1 || '.orders')::regclass
			  AND attname = $2
			  AND NOT attisdropped)`,
		f.schema, column).Scan(&exists))
	return exists
}

func (f destructiveFixture) hasConstraint(t *testing.T, name string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT EXISTS (
			SELECT 1 FROM pg_constraint
			WHERE conrelid = ($1 || '.orders')::regclass
			  AND conname = $2)`,
		f.schema, name).Scan(&exists))
	return exists
}

func (f destructiveFixture) notNull(t *testing.T, column string) bool {
	t.Helper()
	var notNull bool
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT attnotnull FROM pg_attribute
		WHERE attrelid = ($1 || '.orders')::regclass
		  AND attname = $2`,
		f.schema, column).Scan(&notNull))
	return notNull
}

func (f destructiveFixture) columnType(t *testing.T, column string) string {
	t.Helper()
	var typ string
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT data_type FROM information_schema.columns
		WHERE table_schema = $1
		  AND table_name = 'orders'
		  AND column_name = $2`,
		f.schema, column).Scan(&typ))
	return typ
}

// assertDestructiveRefusal pins the refusal contract both front doors share:
// reason destructive-change, class by-design, a nil error, and the table
// named in the detail's acknowledgement. Nothing the gate refuses executes,
// so each caller also asserts the durable state the statement would have
// changed.
func assertDestructiveRefusal(t *testing.T, f destructiveFixture, v verdict.Verdict, err error) {
	t.Helper()
	require.NoError(t, err, "a refusal is a verdict, not an error")
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonDestructiveChange, v.Reason)
	assert.Equal(t, verdict.ClassByDesign, v.Class)
	assert.Empty(t, v.Owner)
	assert.Equal(t, f.table(), v.Table)
	assert.False(t, v.AllowedDestructive)
	assert.Contains(t, v.Detail, "--allow-destructive "+f.table())
	refusal, err := v.Refusal()
	require.NoError(t, err, "the verdict carries its typed refusal proof")
	assert.False(t, verdict.AcceptedBlockingEligible(refusal), "accept-blocking cannot run a destructive statement")
}

// A DROP COLUMN discards every value the column holds, so without the
// acknowledgement it is refused before anything runs, although PostgreSQL
// would drop the column as an instant catalog change.
func TestRunRefusesAnUnacknowledgedDropColumn(t *testing.T) {
	f := newDestructiveFixture(t)

	v, err := f.run(t, `ALTER TABLE {schema}.orders DROP COLUMN note`, runOptions())

	assertDestructiveRefusal(t, f, v, err)
	assert.Empty(t, v.Guidance, "nothing is added back, so there is no in-place change to point at")
	assert.True(t, f.hasColumn(t, "note"), "the column and its values are still there")
}

// A DROP CONSTRAINT discards a guarantee the schema was providing, so it
// is refused without the acknowledgement like a dropped column.
func TestRunRefusesAnUnacknowledgedDropConstraint(t *testing.T) {
	f := newDestructiveFixture(t)

	v, err := f.run(t, `ALTER TABLE {schema}.orders DROP CONSTRAINT orders_qty_positive`, runOptions())

	assertDestructiveRefusal(t, f, v, err)
	assert.True(t, f.hasConstraint(t, "orders_qty_positive"), "the constraint is still there")
}

// DROP NOT NULL discards the same guarantee its constraint form would, so
// it is refused without the acknowledgement too.
func TestRunRefusesAnUnacknowledgedDropNotNull(t *testing.T) {
	f := newDestructiveFixture(t)

	v, err := f.run(t, `ALTER TABLE {schema}.orders ALTER COLUMN note DROP NOT NULL`, runOptions())

	assertDestructiveRefusal(t, f, v, err)
	assert.True(t, f.notNull(t, "note"), "the column is still NOT NULL")
}

// With the acknowledgement naming the table, the DROP COLUMN runs its
// normal native path, and the verdict records the acknowledgement.
func TestRunExecutesAnAcknowledgedDropColumn(t *testing.T) {
	f := newDestructiveFixture(t)
	opts := runOptions()
	opts.AllowDestructive = f.table()

	v, err := f.run(t, `ALTER TABLE {schema}.orders DROP COLUMN note`, opts)

	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	assert.True(t, v.AllowedDestructive)
	assert.False(t, v.Forced, "the destructive acknowledgement is not an override of the routing")
	assert.False(t, f.hasColumn(t, "note"))

	raw, err := v.JSON()
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &wire))
	assert.Equal(t, true, wire["allowed_destructive"], "the acknowledgement is part of the JSON contract")
}

// An acknowledged DROP CONSTRAINT runs and records the acknowledgement.
func TestRunExecutesAnAcknowledgedDropConstraint(t *testing.T) {
	f := newDestructiveFixture(t)
	opts := runOptions()
	opts.AllowDestructive = f.table()

	v, err := f.run(t, `ALTER TABLE {schema}.orders DROP CONSTRAINT orders_qty_positive`, opts)

	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	assert.True(t, v.AllowedDestructive)
	assert.False(t, f.hasConstraint(t, "orders_qty_positive"))
}

// An acknowledged DROP NOT NULL runs and records the acknowledgement.
func TestRunExecutesAnAcknowledgedDropNotNull(t *testing.T) {
	f := newDestructiveFixture(t)
	opts := runOptions()
	opts.AllowDestructive = f.table()

	v, err := f.run(t, `ALTER TABLE {schema}.orders ALTER COLUMN note DROP NOT NULL`, opts)

	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	assert.True(t, v.AllowedDestructive)
	assert.False(t, f.notNull(t, "note"))
}

// A statement that discards nothing runs as it always did with the
// acknowledgement present, and its verdict claims no acknowledged loss.
func TestRunIgnoresTheAcknowledgementForANonDestructiveStatement(t *testing.T) {
	f := newDestructiveFixture(t)
	opts := runOptions()
	opts.AllowDestructive = f.table()

	v, err := f.run(t, `ALTER TABLE {schema}.orders ADD COLUMN sku text`, opts)

	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	assert.False(t, v.AllowedDestructive)
	assert.True(t, f.hasColumn(t, "sku"))
}

// An acknowledgement that names another table is a usage error with no
// verdict, and nothing executes: the typed acknowledgement exists so a
// copied command cannot discard a different table's data.
func TestRunRejectsAMismatchedDestructiveAcknowledgement(t *testing.T) {
	f := newDestructiveFixture(t)
	opts := runOptions()
	opts.AllowDestructive = "public.orders"

	v, err := f.run(t, `ALTER TABLE {schema}.orders DROP COLUMN note`, opts)

	require.ErrorIs(t, err, migrate.ErrAllowDestructiveMismatch)
	assert.Contains(t, err.Error(), f.table(), "the error names the table the acknowledgement must name")
	assert.Equal(t, verdict.Verdict{}, v, "a usage error carries no verdict")
	assert.True(t, f.hasColumn(t, "note"), "nothing was executed")
}

// The gate covers every route: a statement that drops a column and
// retypes another routes to copy-and-swap, and without the acknowledgement
// it is refused before any shadow table is built.
func TestRunRefusesAnUnacknowledgedDropColumnRoutedToCopyAndSwap(t *testing.T) {
	f := newDestructiveFixture(t)
	const alter = `
		ALTER TABLE {schema}.orders
			DROP COLUMN note,
			ALTER COLUMN qty TYPE bigint`

	v, err := f.run(t, alter, f.copySwapOptions())

	assertDestructiveRefusal(t, f, v, err)
	assert.True(t, f.hasColumn(t, "note"), "the column is still there")
	assert.Equal(t, "integer", f.columnType(t, "qty"), "the retype did not run either")
	var tables int
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1
		  AND c.relkind = 'r'`,
		f.schema).Scan(&tables))
	assert.Equal(t, 1, tables, "no shadow table was built")
}

// With the acknowledgement the same statement runs through copy-and-swap:
// the destructive acknowledgement lifts the gate and nothing else.
func TestRunExecutesAnAcknowledgedDropColumnThroughCopyAndSwap(t *testing.T) {
	f := newDestructiveFixture(t)
	opts := f.copySwapOptions()
	opts.AllowDestructive = f.table()
	const alter = `
		ALTER TABLE {schema}.orders
			DROP COLUMN note,
			ALTER COLUMN qty TYPE bigint`

	v, err := f.run(t, alter, opts)

	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome)
	assert.True(t, v.AllowedDestructive)
	assert.False(t, f.hasColumn(t, "note"))
	assert.Equal(t, "bigint", f.columnType(t, "qty"))
}

// --force and the destructive acknowledgement accept different risks. A
// forced run of a type rewrite that also drops a column is still refused
// for the data loss, and runs once both are named.
func TestRunNeedsBothForceAndTheDestructiveAcknowledgement(t *testing.T) {
	f := newDestructiveFixture(t)
	const alter = `
		ALTER TABLE {schema}.orders
			DROP COLUMN note,
			ALTER COLUMN qty TYPE bigint`

	forced := runOptions()
	forced.Force = f.table()
	v, err := f.run(t, alter, forced)
	assertDestructiveRefusal(t, f, v, err)
	assert.True(t, f.hasColumn(t, "note"), "force alone does not acknowledge the loss")

	both := runOptions()
	both.Force = f.table()
	both.AllowDestructive = f.table()
	v, err = f.run(t, alter, both)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome, "the forced form runs natively, not through copy-and-swap")
	assert.True(t, v.Forced)
	assert.True(t, v.AllowedDestructive)
	assert.False(t, f.hasColumn(t, "note"))
	assert.Equal(t, "bigint", f.columnType(t, "qty"))
}

// Dropping a column and adding it back under the same name in one ALTER
// discards every value and leaves the column empty, which is rarely what a
// change of type means. The refusal carries the typed guidance to change
// the column in place, which keeps the values.
func TestRunGuidesADropAndReAddToAnInPlaceTypeChange(t *testing.T) {
	f := newDestructiveFixture(t)
	const alter = `
		ALTER TABLE {schema}.orders
			DROP COLUMN qty,
			ADD COLUMN qty bigint`

	v, err := f.run(t, alter, runOptions())

	assertDestructiveRefusal(t, f, v, err)
	assert.Equal(t, suggest.GuidanceAlterColumnTypeInPlace, v.Guidance)
	assert.Contains(t, v.Detail, "ALTER COLUMN ... TYPE ... USING")
	var total int64
	require.NoError(t, f.pool.QueryRow(t.Context(),
		"SELECT sum(qty) FROM "+f.schema+".orders").Scan(&total))
	assert.EqualValues(t, 500*501/2, total, "every qty value is still there")
}

// The destructive acknowledgement alone does not lift a DROP INDEX: the
// statement-kind gate refuses it first in favor of DROP INDEX
// CONCURRENTLY, and only --accept-blocking accepts that refusal.
func TestRunAllowDestructiveDoesNotLiftADropIndexRefusal(t *testing.T) {
	f := newDestructiveFixture(t)
	opts := runOptions()
	opts.AllowDestructive = f.table()

	v, err := f.run(t, `DROP INDEX {schema}.orders_qty_idx`, opts)

	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonIndexStatement, v.Reason)
	assert.False(t, v.AllowedDestructive)
	var exists bool
	require.NoError(t, f.pool.QueryRow(t.Context(),
		"SELECT to_regclass($1) IS NOT NULL", f.schema+".orders_qty_idx").Scan(&exists))
	assert.True(t, exists, "the index is still there")
}
