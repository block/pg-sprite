package diffplan_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/verdict"
)

// A desired file that widens a column a trigger's UPDATE OF list names,
// and retypes a partition key column, plans both changes as refused
// statements naming the trigger and the partitioned table, since
// PostgreSQL would refuse each. The rest of the file stays planned as
// usual.
func TestPlanRefusesChangingAColumnHiddenDependentsNeed(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE FUNCTION %[1]s.noop() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;
		CREATE TABLE %[1]s.orders (id bigint PRIMARY KEY, status varchar(16));
		CREATE TRIGGER audit_status BEFORE UPDATE OF status ON %[1]s.orders
			FOR EACH ROW EXECUTE FUNCTION %[1]s.noop();
		CREATE TABLE %[1]s.events (id bigint, region varchar(8)) PARTITION BY LIST (region)`, schema))
	require.NoError(t, err)

	for _, tc := range []struct {
		name, desired, refused, planned string
		check                           func(t *testing.T, d planner.Decision)
	}{
		{
			name:    "trigger",
			desired: `CREATE TABLE orders (id bigint PRIMARY KEY, status varchar(64), note text);`,
			refused: fmt.Sprintf("ALTER TABLE %s.orders ALTER COLUMN status TYPE varchar(64)", schema),
			planned: fmt.Sprintf("ALTER TABLE %s.orders ADD COLUMN note text", schema),
			check: func(t *testing.T, d planner.Decision) {
				assert.Equal(t, planner.ReasonDependentTrigger, d.Reason)
				assert.Equal(t, []string{fmt.Sprintf(`"audit_status" ON "%s"."orders"`, schema)}, d.DependentTriggers)
			},
		},
		{
			name:    "partition key",
			desired: `CREATE TABLE events (id bigint, region varchar(16), kind text) PARTITION BY LIST (region);`,
			refused: fmt.Sprintf("ALTER TABLE %s.events ALTER COLUMN region TYPE varchar(16)", schema),
			planned: fmt.Sprintf("ALTER TABLE %s.events ADD COLUMN kind text", schema),
			check: func(t *testing.T, d planner.Decision) {
				assert.Equal(t, planner.ReasonPartitionKey, d.Reason)
				assert.Equal(t, []string{fmt.Sprintf(`"%s"."events"`, schema)}, d.PartitionKeyTables)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: parseDesired(t, tc.desired)})
			require.NoError(t, err)
			assert.Equal(t, router.DispositionRefuse, report.Disposition)
			var found, addPlanned bool
			for _, st := range report.Statements {
				switch st.SQL {
				case tc.refused:
					found = true
					assert.Equal(t, planner.RouteRefuse, st.Route)
					assert.Equal(t, verdict.ReasonUnsupportedStatement, st.Reason)
					assert.Empty(t, st.ExecSQL)
					require.Len(t, st.Decisions, 1)
					tc.check(t, st.Decisions[0])
				case tc.planned:
					addPlanned = true
					assert.Equal(t, planner.RouteNative, st.Route)
				}
			}
			assert.True(t, found, "the type change is planned and refused, got %v", report.Statements)
			assert.True(t, addPlanned, "the unrelated column add is still planned, got %v", report.Statements)
		})
	}
}

// A desired file that removes a generated column and widens the column it
// reads plans the widening as the relabel it is: the drop comes first in
// the plan, so the generated column no longer blocks the type change. A
// file that keeps the generated column still has the widening refused.
func TestPlanWidensAColumnOnceItsGeneratedColumnIsDropped(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %s.ledger (id bigint PRIMARY KEY, amount numeric(10, 2), total numeric GENERATED ALWAYS AS (amount * 2) STORED)`, schema))
	require.NoError(t, err)
	widen := fmt.Sprintf("ALTER TABLE %s.ledger ALTER COLUMN amount TYPE numeric(12, 2)", schema)

	report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: parseDesired(t,
		`CREATE TABLE ledger (id bigint PRIMARY KEY, amount numeric(12, 2));`)})
	require.NoError(t, err)
	var sqls []string
	for _, st := range report.Statements {
		sqls = append(sqls, st.SQL)
		if st.SQL == widen {
			require.Len(t, st.Decisions, 1)
			assert.Equal(t, planner.RouteNative, st.Route)
			assert.Equal(t, planner.ReasonBinaryCoercible, st.Decisions[0].Reason)
			assert.Empty(t, st.Decisions[0].DependentGeneratedColumns)
		}
	}
	assert.Equal(t, []string{fmt.Sprintf("ALTER TABLE %s.ledger DROP total", schema), widen}, sqls)
	assert.NotEqual(t, router.DispositionRefuse, report.Disposition)

	report, err = diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: parseDesired(t,
		`CREATE TABLE ledger (id bigint PRIMARY KEY, amount numeric(12, 2), total numeric GENERATED ALWAYS AS (amount * 2) STORED);`)})
	require.NoError(t, err)
	require.Len(t, report.Statements, 1)
	assert.Equal(t, widen, report.Statements[0].SQL)
	require.Len(t, report.Statements[0].Decisions, 1)
	assert.Equal(t, planner.ReasonDependentGeneratedColumn, report.Statements[0].Decisions[0].Reason)
}

// A desired file that removes a column and the generated column reading it
// plans the generated column's drop first, so neither drop is refused. The
// server is the oracle: the plan's statements run in order.
func TestPlanDropsAGeneratedColumnBeforeTheColumnItReads(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %s.ledger (id bigint PRIMARY KEY, amount numeric(10, 2), total numeric GENERATED ALWAYS AS (amount * 2) STORED)`, schema))
	require.NoError(t, err)

	report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: parseDesired(t,
		`CREATE TABLE ledger (id bigint PRIMARY KEY);`)})
	require.NoError(t, err)
	var sqls []string
	for _, st := range report.Statements {
		sqls = append(sqls, st.SQL)
		assert.NotEqual(t, planner.RouteRefuse, st.Route, st.SQL)
	}
	require.Equal(t, []string{
		fmt.Sprintf("ALTER TABLE %s.ledger DROP total", schema),
		fmt.Sprintf("ALTER TABLE %s.ledger DROP amount", schema),
	}, sqls)
	assert.NotEqual(t, router.DispositionRefuse, report.Disposition)

	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
	for _, sql := range sqls {
		_, err := tx.Exec(t.Context(), sql)
		require.NoError(t, err, sql)
	}
}
