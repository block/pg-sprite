package migrate_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// parseEnumType parses a desired enum type file.
func parseEnumType(t *testing.T, sql string) statement.DesiredEnumType {
	t.Helper()
	file, err := statement.ParseDesiredFile(sql)
	require.NoError(t, err)
	desired, ok := file.EnumType()
	require.True(t, ok, "the file declares an enum type")
	return desired
}

// liveEnumLabels reads the labels of schema.name in sort order.
func liveEnumLabels(t *testing.T, pool *pgxpool.Pool, schema, name string) []string {
	t.Helper()
	live, err := schemadiff.IntrospectEnumType(t.Context(), pool, schema, name)
	require.NoError(t, err)
	return live.Labels
}

// A desired enum type file that lists a new label at the end and another
// ahead of a live one plans one native ADD VALUE per label, placing the
// insert BEFORE the live label it precedes. The run adds both, the live
// type then sorts every label in the file's order, and a second run finds
// nothing to do.
func TestRunDesiredEnumTypeAddsLabelsInDeclaredOrder(t *testing.T) {
	pool := enumPool(t)
	schema := testutil.NewSchema(t, pool)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE TYPE %s.order_status AS ENUM ('new', 'shipped')`, schema))
	require.NoError(t, err)
	desired := parseEnumType(t, `CREATE TYPE order_status AS ENUM ('new', 'paid', 'shipped', 'delivered');`)

	report, err := diffplan.PlanEnumType(t.Context(), pool, schema, desired)
	require.NoError(t, err)
	assert.Equal(t, schema, report.Schema)
	assert.Equal(t, "order_status", report.Type)
	assert.Empty(t, report.Table)
	assert.Equal(t, router.DispositionExecute, report.Disposition)
	require.Len(t, report.Statements, 2)
	assert.Equal(t, fmt.Sprintf("ALTER TYPE %s.order_status ADD VALUE 'paid' BEFORE 'shipped'", schema), report.Statements[0].SQL)
	assert.Equal(t, fmt.Sprintf("ALTER TYPE %s.order_status ADD VALUE 'delivered'", schema), report.Statements[1].SQL)
	for _, st := range report.Statements {
		assert.Equal(t, schemadiff.ChangeAddEnumValue, st.Kind)
		assert.Equal(t, planner.RouteNative, st.Route)
		assert.Equal(t, router.BackendNative, st.Backend)
		assert.False(t, st.Destructive)
	}

	res, err := migrate.RunDesiredEnumType(t.Context(), pool,
		migrate.EnumTypeRequest{Schema: schema, Desired: desired, ExpectedFingerprint: report.Fingerprint}, runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, res.Outcome, "%s: %s", res.Reason, res.Detail)
	require.Len(t, res.Verdicts, 2)
	for _, v := range res.Verdicts {
		assert.Equal(t, verdict.OutcomeExecuted, v.Outcome, v.Detail)
	}
	assert.Equal(t, []string{"new", "paid", "shipped", "delivered"}, liveEnumLabels(t, pool, schema, "order_status"))

	again, err := migrate.RunDesiredEnumType(t.Context(), pool,
		migrate.EnumTypeRequest{Schema: schema, Desired: desired}, runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, again.Outcome)
	assert.Empty(t, again.Plan.Statements)
	assert.Empty(t, again.Verdicts)
	assert.Contains(t, again.Detail, "already converged")
}

// A desired enum type file the live type cannot converge onto is refused
// before anything runs, classed by why: a type that does not exist yet or a
// label the file dropped is the owner's to fix directly, labels listed out
// of live order are the file's to correct, and a name that holds a table's
// row type is not an enum at all. The live type keeps its labels.
func TestRunDesiredEnumTypeRefusesWhatItCannotConverge(t *testing.T) {
	pool := enumPool(t)
	tests := []struct {
		name    string
		fixture string
		file    string
		cause   string
		class   verdict.Class
		owner   verdict.Owner
	}{
		{"missing type", "", `CREATE TYPE order_status AS ENUM ('new');`,
			"does not exist, and a plan never creates a type", verdict.ClassNoOnlineSafetyProblem, verdict.OwnerDirectOperator},
		{"label removed", `CREATE TYPE %s.order_status AS ENUM ('new', 'paid', 'shipped')`,
			`CREATE TYPE order_status AS ENUM ('new', 'shipped');`,
			"carries ('paid'), which the file no longer lists", verdict.ClassNoOnlineSafetyProblem, verdict.OwnerDirectOperator},
		{"label renamed", `CREATE TYPE %s.order_status AS ENUM ('new', 'paid')`,
			`CREATE TYPE order_status AS ENUM ('new', 'settled');`,
			"carries ('paid'), which the file no longer lists", verdict.ClassNoOnlineSafetyProblem, verdict.OwnerDirectOperator},
		{"labels reordered", `CREATE TYPE %s.order_status AS ENUM ('new', 'paid')`,
			`CREATE TYPE order_status AS ENUM ('paid', 'new', 'shipped');`,
			"sorts its labels as ('new', 'paid'), and the file lists them as ('paid', 'new')", verdict.ClassByDesign, ""},
		{"table row type", `CREATE TABLE %s.order_status (id int)`, `CREATE TYPE order_status AS ENUM ('new');`,
			"is a type that is not an enum", verdict.ClassCapabilityBoundary, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			schema := testutil.NewSchema(t, pool)
			if tt.fixture != "" {
				_, err := pool.Exec(t.Context(), fmt.Sprintf(tt.fixture, schema))
				require.NoError(t, err)
			}
			var before []string
			if tt.class != verdict.ClassCapabilityBoundary && tt.fixture != "" {
				before = liveEnumLabels(t, pool, schema, "order_status")
			}
			desired := parseEnumType(t, tt.file)

			_, err := diffplan.PlanEnumType(t.Context(), pool, schema, desired)
			require.ErrorIs(t, err, schemadiff.ErrUnsupportedChange)

			res, err := migrate.RunDesiredEnumType(t.Context(), pool,
				migrate.EnumTypeRequest{Schema: schema, Desired: desired}, runOptions())
			require.NoError(t, err)
			assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
			assert.Equal(t, verdict.ReasonUnsupportedStatement, res.Reason)
			assert.Equal(t, tt.class, res.Class)
			assert.Equal(t, tt.owner, res.Owner)
			assert.Contains(t, res.Detail, tt.cause)
			assert.Contains(t, res.Detail, "nothing was planned or executed")
			assert.Empty(t, res.Verdicts)
			if before != nil {
				assert.Equal(t, before, liveEnumLabels(t, pool, schema, "order_status"), "nothing was added")
			}
		})
	}
}

// A plan pinned to a reviewed fingerprint runs only while the live type
// still yields that plan: once another session adds a label the review did
// not see, the plan changes and the run is refused before anything runs.
func TestRunDesiredEnumTypeRefusesAStalePlan(t *testing.T) {
	pool := enumPool(t)
	schema := testutil.NewSchema(t, pool)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE TYPE %s.order_status AS ENUM ('new')`, schema))
	require.NoError(t, err)
	desired := parseEnumType(t, `CREATE TYPE order_status AS ENUM ('new', 'paid', 'shipped');`)
	reviewed, err := diffplan.PlanEnumType(t.Context(), pool, schema, desired)
	require.NoError(t, err)
	require.Len(t, reviewed.Statements, 2)

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`ALTER TYPE %s.order_status ADD VALUE 'shipped'`, schema))
	require.NoError(t, err)

	res, err := migrate.RunDesiredEnumType(t.Context(), pool,
		migrate.EnumTypeRequest{Schema: schema, Desired: desired, ExpectedFingerprint: reviewed.Fingerprint}, runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
	assert.Equal(t, verdict.ReasonPlanFingerprintMismatch, res.Reason)
	assert.Empty(t, res.Verdicts)
	assert.Equal(t, []string{"new", "shipped"}, liveEnumLabels(t, pool, schema, "order_status"))
}
