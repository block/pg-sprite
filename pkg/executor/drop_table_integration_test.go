package executor_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/schemadiff"
)

var dropTableBudget = executor.Budget{LockTimeout: 500 * time.Millisecond, StatementTimeout: 5 * time.Second}

// plannedTableDefinition reads the definition a plan records for dropping
// schema.table.
func plannedTableDefinition(t *testing.T, pool *pgxpool.Pool, schema, table string) string {
	t.Helper()
	m, err := schemadiff.Introspect(t.Context(), pool, schema, table)
	require.NoError(t, err)
	return schemadiff.DroppedTableDefinition(m)
}

func dropTable(t *testing.T, pool *pgxpool.Pool, schema, table, definition string) error {
	t.Helper()
	return executor.ExecuteDropTable(t.Context(), pool, schema, table, definition, dropTableBudget, executor.DefaultRetryPolicy())
}

// A reviewed table drop removes the table with its rows, indexes,
// constraints, and owned sequence, and leaves the table its foreign key
// references alone.
func TestExecuteDropTableDropsAnUnchangedTable(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.parent (id int PRIMARY KEY);
		CREATE TABLE %[1]s.t (id serial PRIMARY KEY, parent_id int REFERENCES %[1]s.parent, qty int CHECK (qty > 0));
		CREATE INDEX t_qty_idx ON %[1]s.t (qty);
		INSERT INTO %[1]s.parent VALUES (1);
		INSERT INTO %[1]s.t (parent_id, qty) VALUES (1, 5)`, schema))
	require.NoError(t, err)

	require.NoError(t, dropTable(t, pool, schema, "t", plannedTableDefinition(t, pool, schema, "t")))
	assert.False(t, relationExists(t, pool, schema, "t"))
	assert.False(t, relationExists(t, pool, schema, "t_id_seq"), "the owned sequence goes with the table")
	assert.True(t, relationExists(t, pool, schema, "parent"))
}

// A plan that drops a referencing table and the table it references drops
// the referencing table first. The referenced table's definition then lists
// the foreign key once, as a "referenced by" line, so the plan records it
// without that line and the drop that follows finds the table unchanged.
func TestExecuteDropTableAfterItsReferencingTable(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.regions (id int PRIMARY KEY);
		CREATE TABLE %[1]s.warehouses (id int PRIMARY KEY, region_id int REFERENCES %[1]s.regions)`, schema))
	require.NoError(t, err)

	warehouses := plannedTableDefinition(t, pool, schema, "warehouses")
	regionsModel, err := schemadiff.Introspect(t.Context(), pool, schema, "regions")
	require.NoError(t, err)
	require.Equal(t, []string{"warehouses.warehouses_region_id_fkey"}, regionsModel.ReferencedBy)
	assert.NotContains(t, schemadiff.DroppedTableDefinition(regionsModel), "on table warehouses",
		"the incoming foreign key is listed once, as a referenced-by line")
	regionsModel.ReferencedBy = nil
	regions := schemadiff.DroppedTableDefinition(regionsModel)

	require.NoError(t, dropTable(t, pool, schema, "warehouses", warehouses))
	require.NoError(t, dropTable(t, pool, schema, "regions", regions))
	assert.False(t, relationExists(t, pool, schema, "regions"))
}

// A table that changed after planning is not the table the reviewer
// accepted losing: a column added, a table dropped and created again with
// another shape, a foreign key of another table that now references it, an
// object the drop would remove with the table (a trigger, a rule, a
// publication membership, row security, or a policy), and another kind of
// relation that took the name all refuse, dropping nothing.
func TestExecuteDropTableRefusesATableThatChanged(t *testing.T) {
	pool, schema := newPool(t)
	setup := func(sql string) {
		_, err := pool.Exec(t.Context(), fmt.Sprintf(sql, schema))
		require.NoError(t, err)
	}
	setup(`CREATE TABLE %[1]s.t (id int PRIMARY KEY, note text);
		CREATE FUNCTION %[1]s.noop() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'`)
	cases := []struct {
		name   string
		change string
	}{
		{"column added", `ALTER TABLE %[1]s.t ADD COLUMN extra int`},
		{"recreated with another shape", `DROP TABLE %[1]s.t; CREATE TABLE %[1]s.t (id bigint PRIMARY KEY, note text)`},
		{"referenced by another table", `CREATE TABLE %[1]s.child (t_id int REFERENCES %[1]s.t)`},
		{"trigger added", `CREATE TRIGGER t_audit BEFORE INSERT ON %[1]s.t FOR EACH ROW EXECUTE FUNCTION %[1]s.noop()`},
		{"rule added", `CREATE RULE t_ignore_deletes AS ON DELETE TO %[1]s.t DO INSTEAD NOTHING`},
		{"added to a publication", `CREATE PUBLICATION ` + schema + `_pub FOR TABLE %[1]s.t`},
		{"row security enabled", `ALTER TABLE %[1]s.t ENABLE ROW LEVEL SECURITY`},
		{"policy added", `CREATE POLICY t_all ON %[1]s.t USING (true)`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setup(`DROP PUBLICATION IF EXISTS ` + schema + `_pub; DROP TABLE IF EXISTS %[1]s.child; DROP TABLE IF EXISTS %[1]s.t;
				CREATE TABLE %[1]s.t (id int PRIMARY KEY, note text)`)
			definition := plannedTableDefinition(t, pool, schema, "t")
			setup(tc.change)

			err := dropTable(t, pool, schema, "t", definition)
			require.ErrorIs(t, err, executor.ErrDroppedObjectDrift)
			assert.Equal(t, executor.CodeDroppedObjectDrift, executor.OutcomeCode(err))
			assert.True(t, relationExists(t, pool, schema, "t"), "a refused drop drops nothing")
		})
	}

	replacements := []struct {
		name    string
		replace string
		remove  string
	}{
		{"replaced by a view", `CREATE VIEW %[1]s.t AS SELECT 1 AS id`, `DROP VIEW %[1]s.t`},
		{"replaced by a materialized view", `CREATE MATERIALIZED VIEW %[1]s.t AS SELECT 1 AS id`, `DROP MATERIALIZED VIEW %[1]s.t`},
		{"replaced by a sequence", `CREATE SEQUENCE %[1]s.t`, `DROP SEQUENCE %[1]s.t`},
	}
	for _, tc := range replacements {
		t.Run(tc.name, func(t *testing.T) {
			setup(`DROP PUBLICATION IF EXISTS ` + schema + `_pub; DROP TABLE IF EXISTS %[1]s.child; DROP TABLE IF EXISTS %[1]s.t;
				CREATE TABLE %[1]s.t (id int PRIMARY KEY, note text)`)
			definition := plannedTableDefinition(t, pool, schema, "t")
			setup(`DROP TABLE %[1]s.t; ` + tc.replace)

			err := dropTable(t, pool, schema, "t", definition)
			require.ErrorIs(t, err, executor.ErrDroppedObjectDrift)
			assert.Equal(t, executor.CodeDroppedObjectDrift, executor.OutcomeCode(err))
			assert.True(t, relationExists(t, pool, schema, "t"))
			setup(tc.remove)
		})
	}
}

// A table that is already gone, or whose schema is, reports table-not-found
// so the caller can tell it from a drop that failed.
func TestExecuteDropTableReportsAMissingTable(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s.t (id int PRIMARY KEY)`, schema))
	require.NoError(t, err)
	definition := plannedTableDefinition(t, pool, schema, "t")
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`DROP TABLE %s.t`, schema))
	require.NoError(t, err)

	err = dropTable(t, pool, schema, "t", definition)
	require.ErrorIs(t, err, executor.ErrTableNotFound)
	assert.Equal(t, executor.CodeTableNotFound, executor.OutcomeCode(err))

	err = dropTable(t, pool, schema+"_missing", "t", definition)
	require.ErrorIs(t, err, executor.ErrTableNotFound)
}

// A drop that would reach tables below the named one, or change the table
// above it, is refused even when its definition is unchanged: partitioned
// tables and inheritance parents would take their children with them, and
// partitions and inheritance children hold rows of the table above.
func TestExecuteDropTableRefusesTablesInAHierarchy(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.events (id int, at date) PARTITION BY RANGE (at);
		CREATE TABLE %[1]s.events_2026 PARTITION OF %[1]s.events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
		CREATE TABLE %[1]s.base (id int);
		CREATE TABLE %[1]s.derived (extra int) INHERITS (%[1]s.base)`, schema))
	require.NoError(t, err)

	cases := []struct {
		table string
		want  error
		code  executor.Code
	}{
		{"events", executor.ErrDroppedObjectHasDescendants, executor.CodeDroppedObjectHasDescendants},
		{"base", executor.ErrDroppedObjectHasDescendants, executor.CodeDroppedObjectHasDescendants},
		{"events_2026", executor.ErrDroppedTableHasParent, executor.CodeDroppedTableHasParent},
		{"derived", executor.ErrDroppedTableHasParent, executor.CodeDroppedTableHasParent},
	}
	for _, tc := range cases {
		t.Run(tc.table, func(t *testing.T) {
			err := dropTable(t, pool, schema, tc.table, plannedTableDefinition(t, pool, schema, tc.table))
			require.ErrorIs(t, err, tc.want)
			assert.Equal(t, tc.code, executor.OutcomeCode(err))
			assert.True(t, relationExists(t, pool, schema, tc.table), "a refused drop drops nothing")
		})
	}
}

// A view that reads the table makes the server refuse the drop, because
// the drop never says CASCADE; the table and the view both remain.
func TestExecuteDropTableRefusesATableAViewReads(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.t (id int PRIMARY KEY, note text);
		CREATE VIEW %[1]s.t_notes AS SELECT note FROM %[1]s.t`, schema))
	require.NoError(t, err)

	err = dropTable(t, pool, schema, "t", plannedTableDefinition(t, pool, schema, "t"))
	require.ErrorIs(t, err, executor.ErrDroppedTableHasDependents)
	assert.Equal(t, executor.CodeDroppedTableHasDependents, executor.OutcomeCode(err))
	assert.True(t, relationExists(t, pool, schema, "t"))
	assert.True(t, relationExists(t, pool, schema, "t_notes"))
}

// A table another transaction is reading cannot be locked within the lock
// budget; every attempt gives up its place in the lock queue, and the
// table remains.
func TestExecuteDropTableLockBudgetExpiry(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s.t (id int PRIMARY KEY)`, schema))
	require.NoError(t, err)
	definition := plannedTableDefinition(t, pool, schema, "t")
	holdOldTransaction(t, pool, schema)

	tight := executor.Budget{LockTimeout: 50 * time.Millisecond, StatementTimeout: 5 * time.Second}
	retry := executor.RetryPolicy{MaxAttempts: 2, InitialBackoff: 10 * time.Millisecond, MaxBackoff: 10 * time.Millisecond}
	err = executor.ExecuteDropTable(t.Context(), pool, schema, "t", definition, tight, retry)
	var budgetErr *executor.BudgetError
	require.ErrorAs(t, err, &budgetErr)
	assert.Equal(t, executor.CauseLock, budgetErr.Cause)
	assert.Equal(t, 2, budgetErr.Attempts)
	assert.True(t, relationExists(t, pool, schema, "t"))
}

// A drop without the reviewed definition, a schema, or a bounded budget is
// refused before it touches the database.
func TestExecuteDropTableRejectsBadInputs(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s.t (id int PRIMARY KEY)`, schema))
	require.NoError(t, err)
	definition := plannedTableDefinition(t, pool, schema, "t")
	retry := executor.DefaultRetryPolicy()

	err = executor.ExecuteDropTable(t.Context(), pool, schema, "t", "", dropTableBudget, retry)
	require.ErrorIs(t, err, executor.ErrInvariantViolation)
	err = executor.ExecuteDropTable(t.Context(), pool, "", "t", definition, dropTableBudget, retry)
	require.ErrorIs(t, err, executor.ErrInvariantViolation)
	err = executor.ExecuteDropTable(t.Context(), pool, schema, "t", definition, executor.Budget{}, retry)
	require.Error(t, err)
	assert.True(t, relationExists(t, pool, schema, "t"))
}
