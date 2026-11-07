package migrate_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/verdict"
)

// A column that a trigger's UPDATE OF list or WHEN condition names, that
// a generated column reads, or that is in a partition key cannot be
// retyped or dropped; PostgreSQL refuses even a widening it would
// otherwise relabel without a rewrite. The run is refused at plan time
// with the blocking objects named, so the operator learns what to change
// before anything reaches the server, and the column is left as it was.
// A column only a trigger's function body reads still retypes, because
// the server records no dependency on it.
func TestRunRefusesChangingAColumnHiddenDependentsNeed(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	seed := func(t *testing.T) string {
		t.Helper()
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`
			CREATE FUNCTION %[1]s.touch() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN IF NEW.note IS NULL THEN NEW.note := ''; END IF; RETURN NEW; END $$;
			CREATE TABLE %[1]s.orders (
				id bigint PRIMARY KEY,
				status varchar(16),
				qty int,
				amount numeric(10, 2),
				total numeric GENERATED ALWAYS AS (amount * 2) STORED,
				note varchar(100)
			);
			INSERT INTO %[1]s.orders (id, status, qty, amount) VALUES (1, 'new', 1, 9.99);
			CREATE TRIGGER audit_status BEFORE UPDATE OF status ON %[1]s.orders
				FOR EACH ROW EXECUTE FUNCTION %[1]s.touch();
			CREATE TRIGGER positive_qty BEFORE UPDATE ON %[1]s.orders
				FOR EACH ROW WHEN (NEW.qty > 0) EXECUTE FUNCTION %[1]s.touch();
			CREATE TABLE %[1]s.events (id bigint, region varchar(8), kind text) PARTITION BY LIST (region);
			CREATE TABLE %[1]s.events_eu PARTITION OF %[1]s.events FOR VALUES IN ('eu')`, schema))
		require.NoError(t, err)
		return schema
	}

	for _, tc := range []struct {
		name, statement, table, column, columnType string
		reason                                     planner.Reason
		named                                      func(schema string) []string
		field                                      func(v verdict.Verdict) []string
		detail, help                               string
	}{
		{
			name:      "an UPDATE OF trigger blocks a widening",
			statement: "ALTER TABLE %s.orders ALTER COLUMN status TYPE varchar(64)",
			table:     "orders", column: "status", columnType: "character varying(16)",
			named:  func(s string) []string { return []string{fmt.Sprintf(`"audit_status" ON "%s"."orders"`, s)} },
			field:  func(v verdict.Verdict) []string { return v.DependentTriggers },
			detail: "a column named by trigger ", help: planner.DependentFixRecreateAroundChange.Help("triggers"),
		},
		{
			name:      "a WHEN trigger blocks a drop",
			statement: "ALTER TABLE %s.orders DROP COLUMN qty",
			table:     "orders", column: "qty", columnType: "integer",
			named:  func(s string) []string { return []string{fmt.Sprintf(`"positive_qty" ON "%s"."orders"`, s)} },
			field:  func(v verdict.Verdict) []string { return v.DependentTriggers },
			detail: "a column named by trigger ", help: planner.DependentFixRewriteDependents.Help("triggers"),
		},
		{
			name:      "a generated column blocks a widening",
			statement: "ALTER TABLE %s.orders ALTER COLUMN amount TYPE numeric(12, 2)",
			table:     "orders", column: "amount", columnType: "numeric(10,2)",
			named:  func(s string) []string { return []string{fmt.Sprintf(`"%s"."orders"."total"`, s)} },
			field:  func(v verdict.Verdict) []string { return v.DependentGeneratedColumns },
			detail: "a column read by generated column ", help: planner.GeneratedColumnHelp,
		},
		{
			name:      "a partition key blocks a widening",
			statement: "ALTER TABLE %s.events ALTER COLUMN region TYPE varchar(16)",
			table:     "events", column: "region", columnType: "character varying(8)",
			named:  func(s string) []string { return []string{fmt.Sprintf(`"%s"."events"`, s)} },
			field:  func(v verdict.Verdict) []string { return v.PartitionKeyTables },
			detail: "a column in the partition key of ", help: planner.PartitionKeyHelp,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema := seed(t)
			v, err := migrate.Run(t.Context(), pool, parseOne(t, fmt.Sprintf(tc.statement, schema)), runOptions())
			require.NoError(t, err)
			assert.Equal(t, verdict.OutcomeRefused, v.Outcome, v.Detail)
			assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)
			assert.Equal(t, verdict.ClassCapabilityBoundary, v.Class)
			named := tc.named(schema)
			assert.Equal(t, named, tc.field(v))
			assert.Contains(t, v.Detail, tc.detail+named[0]+",")
			assert.True(t, strings.HasSuffix(v.Detail, "; "+tc.help), v.Detail)
			assert.Empty(t, v.ExecutedSQL)
			assert.Equal(t, tc.columnType, columnType(t, pool, schema, tc.table, tc.column))

			st := parseOne(t, fmt.Sprintf(tc.statement, schema))
			facts, err := migrate.LiveFacts(t.Context(), pool, st)
			require.NoError(t, err)
			plan, err := planner.Classify(st.SQL(), facts.Classifier)
			require.NoError(t, err)
			require.Len(t, plan.Decisions, 1)
			assert.Equal(t, planner.RouteRefuse, plan.Decisions[0].Route)
		})
	}

	t.Run("a column only a trigger body reads still retypes", func(t *testing.T) {
		schema := seed(t)
		v, err := migrate.Run(t.Context(), pool,
			parseOne(t, fmt.Sprintf("ALTER TABLE %s.orders ALTER COLUMN note TYPE varchar(200)", schema)), runOptions())
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecuted, v.Outcome, v.Detail)
		assert.Empty(t, v.DependentTriggers)
		assert.Equal(t, "character varying(200)", columnType(t, pool, schema, "orders", "note"))
	})
}
