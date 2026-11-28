package migrate

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// A reviewed plan is executed after another session replaced an object the
// plan drops with a different object of the same name, in the window between
// the pin check and the drop. Each drop re-reads the object it removes and
// refuses with the permanent drift code, drops nothing, and the failed
// verdict says so. Admission cannot see this window, so the check at
// execution is the only thing between the replacement and the drop.
func TestExecutePlanRefusesADropWhoseObjectChangedAfterTheRead(t *testing.T) {
	cases := []struct {
		name    string
		desired string
		replace string
		drift   error
		code    executor.Code
		intact  string
	}{
		{
			name: "column dropped and added again",
			desired: `CREATE TABLE t (
				id int PRIMARY KEY,
				keep text,
				CONSTRAINT t_keep_check CHECK (keep <> '')
			);
			CREATE INDEX t_keep_idx ON t (keep);`,
			replace: `
				ALTER TABLE %[1]s.t DROP COLUMN legacy;
				ALTER TABLE %[1]s.t ADD COLUMN legacy text`,
			drift: executor.ErrDroppedObjectDrift,
			code:  executor.CodeDroppedObjectDrift,
			intact: `
				SELECT count(*)
				FROM information_schema.columns
				WHERE table_schema = '%[1]s'
					AND table_name = 't'
					AND column_name = 'legacy'`,
		},
		{
			name: "index dropped and created again on another column",
			desired: `CREATE TABLE t (
				id int PRIMARY KEY,
				keep text,
				legacy text,
				CONSTRAINT t_keep_check CHECK (keep <> '')
			);`,
			replace: `
				DROP INDEX %[1]s.t_keep_idx;
				CREATE INDEX t_keep_idx ON %[1]s.t (legacy)`,
			drift: executor.ErrDroppedIndexDrift,
			code:  executor.CodeDroppedIndexDrift,
			intact: `
				SELECT count(*)
				FROM pg_indexes
				WHERE schemaname = '%[1]s'
					AND indexname = 't_keep_idx'
					AND indexdef LIKE '%%(legacy)%%'`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
			require.NoError(t, err)
			t.Cleanup(pool.Close)
			schema := testutil.NewSchema(t, pool)
			_, err = pool.Exec(t.Context(), fmt.Sprintf(`
				CREATE TABLE %[1]s.t (
					id int PRIMARY KEY,
					keep text,
					legacy text,
					CONSTRAINT t_keep_check CHECK (keep <> '')
				);
				CREATE INDEX t_keep_idx ON %[1]s.t (keep)`, schema))
			require.NoError(t, err)

			ds, err := statement.ParseDesired(tc.desired)
			require.NoError(t, err)
			report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: ds})
			require.NoError(t, err)
			require.NotEmpty(t, report.Statements)
			var destructive int
			for _, ps := range report.Statements {
				if ps.Destructive {
					destructive++
					require.NotNil(t, ps.Drops, "every destructive statement carries what it drops")
				}
			}
			require.Positive(t, destructive)

			_, err = pool.Exec(t.Context(), fmt.Sprintf(tc.replace, schema))
			require.NoError(t, err)

			opts := DefaultOptions()
			replan := func(ctx context.Context) (plan.Report, error) {
				return diffplan.Plan(ctx, pool, diffplan.Request{Schema: schema, Desired: ds})
			}
			res, err := executePlan(t.Context(), pool, report, opts, schema+".t", "", replan)
			require.ErrorIs(t, err, tc.drift)
			assert.Equal(t, verdict.OutcomeFailed, res.Outcome)
			require.NotEmpty(t, res.Verdicts)
			assert.Equal(t, string(tc.code), res.Verdicts[len(res.Verdicts)-1].Code)

			var n int
			require.NoError(t, pool.QueryRow(t.Context(), fmt.Sprintf(tc.intact, schema)).Scan(&n))
			assert.Equal(t, 1, n, "the replacement must survive the refused drop")
		})
	}
}

// A reviewed plan drops a foreign key, and before it runs another session
// replaces the key with one of the same name on a different column. The drop
// re-reads the key under the table's lock, refuses with the permanent drift
// code, and the replacement stays.
func TestExecutePlanRefusesAForeignKeyDropWhoseKeyChangedAfterTheRead(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.users (id int PRIMARY KEY);
		CREATE TABLE %[1]s.orders (
			id int PRIMARY KEY,
			user_id int CONSTRAINT orders_user_fkey REFERENCES %[1]s.users (id),
			buyer_id int
		)`, schema))
	require.NoError(t, err)

	ds, err := statement.ParseDesired(`CREATE TABLE orders (id int PRIMARY KEY, user_id int, buyer_id int);`)
	require.NoError(t, err)
	report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: ds})
	require.NoError(t, err)
	require.Len(t, report.Statements, 1)
	require.NotNil(t, report.Statements[0].Drops, "the drop carries the key it read")

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		ALTER TABLE %[1]s.orders DROP CONSTRAINT orders_user_fkey;
		ALTER TABLE %[1]s.orders ADD CONSTRAINT orders_user_fkey FOREIGN KEY (buyer_id) REFERENCES %[1]s.users (id)`, schema))
	require.NoError(t, err)

	replan := func(ctx context.Context) (plan.Report, error) {
		return diffplan.Plan(ctx, pool, diffplan.Request{Schema: schema, Desired: ds})
	}
	res, err := executePlan(t.Context(), pool, report, DefaultOptions(), schema+".orders", "", replan)
	require.ErrorIs(t, err, executor.ErrDroppedObjectDrift)
	assert.Equal(t, verdict.OutcomeFailed, res.Outcome)
	require.NotEmpty(t, res.Verdicts)
	assert.Equal(t, string(executor.CodeDroppedObjectDrift), res.Verdicts[len(res.Verdicts)-1].Code)

	var def string
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT pg_get_constraintdef(oid) FROM pg_catalog.pg_constraint
		WHERE conrelid = ($1 || '.orders')::regclass AND conname = 'orders_user_fkey'`, schema).Scan(&def))
	assert.Equal(t, fmt.Sprintf("FOREIGN KEY (buyer_id) REFERENCES %s.users(id)", schema), def, "the replacement key stays")
}
