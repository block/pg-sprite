package diffplan_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemadiff"
)

// A column the desired file makes NOT NULL plans as the online sequence: a
// helper CHECK added NOT VALID, its validation, SET NOT NULL, and the
// helper's drop. The plan records the helper and the definition its add
// leaves as the constraint the sequence validates, so the validation step
// checks it is still that helper, and a caller reading the plan sees the
// validation is the plan's own. It also records the helper as it stands once
// validated as the object the last step drops, so a caller that runs each
// step on its own sends that step through the checked drop and it removes
// the helper. Both definitions match what the catalog prints, including for
// a column whose name needs quoting.
func TestPlanSetNotNullRecordsTheHelperItValidates(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	tests := []struct {
		name       string
		live       string
		desired    string
		constraint string
		def        string
	}{
		{
			name: "a plain column",
			live: `
				CREATE TABLE %s.orders (
					id bigint PRIMARY KEY,
					email text
				)`,
			desired: `
				CREATE TABLE orders (
					id bigint PRIMARY KEY,
					email text NOT NULL
				);`,
			constraint: "orders_email_not_null_check",
			def:        "CHECK ((email IS NOT NULL)) NOT VALID",
		},
		{
			name: "a mixed-case column",
			live: `
				CREATE TABLE %s.orders (
					id bigint PRIMARY KEY,
					"Email" text
				)`,
			desired: `
				CREATE TABLE orders (
					id bigint PRIMARY KEY,
					"Email" text NOT NULL
				);`,
			constraint: "orders_Email_not_null_check",
			def:        `CHECK (("Email" IS NOT NULL)) NOT VALID`,
		},
		{
			name: "a keyword column",
			live: `
				CREATE TABLE %s.orders (
					id bigint PRIMARY KEY,
					"order" text
				)`,
			desired: `
				CREATE TABLE orders (
					id bigint PRIMARY KEY,
					"order" text NOT NULL
				);`,
			constraint: "orders_order_not_null_check",
			def:        `CHECK (("order" IS NOT NULL)) NOT VALID`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			schema := testutil.NewSchema(t, pool)
			_, err := pool.Exec(t.Context(), fmt.Sprintf(tt.live, schema))
			require.NoError(t, err)

			report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: parseDesired(t, tt.desired)})
			require.NoError(t, err)

			require.Len(t, report.Statements, 1)
			st := report.Statements[0]
			assert.Equal(t, schemadiff.ChangeSetNotNull, st.Kind)
			require.Len(t, st.ExecSQL, 4)
			assert.Contains(t, st.ExecSQL[1], "VALIDATE CONSTRAINT")
			assert.Equal(t, &schemadiff.ConstraintValidation{Constraint: tt.constraint, Def: tt.def}, st.Validates)

			// The recorded definition is the one the catalog prints once the
			// helper is added, which the validation step compares it to.
			_, err = pool.Exec(t.Context(), st.ExecSQL[0])
			require.NoError(t, err)
			var def string
			require.NoError(t, pool.QueryRow(t.Context(), `SELECT pg_catalog.pg_get_constraintdef(oid)
				FROM pg_catalog.pg_constraint
				WHERE conrelid = $1::regclass AND conname = $2`, schema+".orders", tt.constraint).Scan(&def))
			assert.Equal(t, tt.def, def)

			// The recorded drop is the helper as the executor reads it
			// once validated and the column flipped, right before the drop
			// step, which then runs through the checked drop gate.
			assert.Equal(t, &schemadiff.DroppedObject{
				Kind:       schemadiff.DroppedConstraint,
				Name:       tt.constraint,
				Definition: strings.TrimSuffix(tt.def, " NOT VALID"),
			}, st.DropsHelper)
			runSteps(t, pool, st.ExecSQL[1:3])
			live, found, err := schemadiff.ReadDroppedObject(t.Context(), pool, schema, "orders", schemadiff.DroppedConstraint, tt.constraint)
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, *st.DropsHelper, live)
			require.NoError(t, runHelperDrop(t, pool, schema, st))
			assert.False(t, constraintExists(t, pool, schema, tt.constraint), "the drop step removes the helper")
			assert.False(t, st.Destructive, "the helper is the plan's own, not live structure")
		})
	}
}

// A caller that runs each step of a SET NOT NULL on its own runs steps 1 to
// 3, then sends the drop step through the checked drop with the recorded
// helper. If another session replaced the helper with a different check
// under its name in between, the drop refuses as drift and the replacement
// stays. If the validation failed on a null row, the helper is still NOT
// VALID, so the drop refuses as drift too; once the row is fixed, the
// sequence resumes at its validation step and the drop then removes the
// helper.
func TestPlanSetNotNullHelperDropRefusesAnotherHelper(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	const (
		desired = `
			CREATE TABLE orders (
				id bigint PRIMARY KEY,
				email text NOT NULL
			);`
		helper = "orders_email_not_null_check"
	)

	t.Run("a helper replaced between the steps", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`
			CREATE TABLE %s.orders (
				id bigint PRIMARY KEY,
				email text
			)`, schema))
		require.NoError(t, err)
		st := planSetNotNull(t, pool, schema, desired)
		runSteps(t, pool, st.ExecSQL[:3])

		_, err = pool.Exec(t.Context(), fmt.Sprintf(`
			ALTER TABLE %[1]s.orders DROP CONSTRAINT %[2]s;
			ALTER TABLE %[1]s.orders ADD CONSTRAINT %[2]s CHECK (email <> '')`, schema, helper))
		require.NoError(t, err)

		require.ErrorIs(t, runHelperDrop(t, pool, schema, st), executor.ErrDroppedObjectDrift)
		assert.True(t, constraintExists(t, pool, schema, helper), "the replacement is not dropped")
	})

	t.Run("a helper a failed validation left NOT VALID", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`
			CREATE TABLE %[1]s.orders (
				id bigint PRIMARY KEY,
				email text
			);
			INSERT INTO %[1]s.orders VALUES (1, 'ada@example.com'), (2, NULL)`, schema))
		require.NoError(t, err)
		st := planSetNotNull(t, pool, schema, desired)
		runSteps(t, pool, st.ExecSQL[:1])
		_, err = pool.Exec(t.Context(), st.ExecSQL[1])
		require.Error(t, err, "the validation fails on the null row")

		require.ErrorIs(t, runHelperDrop(t, pool, schema, st), executor.ErrDroppedObjectDrift)
		assert.True(t, constraintExists(t, pool, schema, helper), "the unvalidated helper is not dropped")

		_, err = pool.Exec(t.Context(), fmt.Sprintf(`UPDATE %s.orders SET email = 'grace@example.com' WHERE id = 2`, schema))
		require.NoError(t, err)
		runSteps(t, pool, st.ExecSQL[1:3])
		require.NoError(t, runHelperDrop(t, pool, schema, st))
		assert.False(t, constraintExists(t, pool, schema, helper), "once validated, the drop removes the helper")
	})
}

// helperDropBudget bounds the checked helper drop in these tests.
var helperDropBudget = executor.SequenceBudget{
	Brief:      executor.Budget{LockTimeout: 500 * time.Millisecond, StatementTimeout: 2 * time.Second},
	Concurrent: executor.ConcurrentBudget{Overall: time.Minute},
	Validate:   executor.ValidateBudget{LockTimeout: time.Second, Overall: time.Minute},
}

// planSetNotNull plans desired against schema and returns its one SET NOT
// NULL statement, which must record the helper it drops.
func planSetNotNull(t *testing.T, pool *pgxpool.Pool, schema, desired string) plan.Statement {
	t.Helper()
	report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: parseDesired(t, desired)})
	require.NoError(t, err)
	require.Len(t, report.Statements, 1)
	st := report.Statements[0]
	require.Len(t, st.ExecSQL, 4)
	require.NotNil(t, st.DropsHelper)
	return st
}

// runSteps runs each planned step as its own statement, as a caller that
// runs the steps on their own does.
func runSteps(t *testing.T, pool *pgxpool.Pool, steps []string) {
	t.Helper()
	for _, sql := range steps {
		_, err := pool.Exec(t.Context(), sql)
		require.NoError(t, err, sql)
	}
}

// runHelperDrop runs the drop step of st through the checked drop with the
// helper st records.
func runHelperDrop(t *testing.T, pool *pgxpool.Pool, schema string, st plan.Statement) error {
	t.Helper()
	pt, err := preflight.CheckTable(t.Context(), pool, schema, "orders", 1<<30)
	require.NoError(t, err)
	_, err = executor.RunSequenceDropping(t.Context(), pool, pt, st.ExecSQL[3:], helperDropBudget, executor.DefaultRetryPolicy(), *st.DropsHelper)
	return err
}

// constraintExists reports whether schema.orders has a constraint named name.
func constraintExists(t *testing.T, pool *pgxpool.Pool, schema, name string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT EXISTS (
		SELECT 1 FROM pg_catalog.pg_constraint
		WHERE conrelid = $1::regclass AND conname = $2)`, schema+".orders", name).Scan(&exists))
	return exists
}
