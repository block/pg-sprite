package migrate_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// ordersWithoutAmt is the desired file for orders once its amt column and
// the CHECK on it are removed.
const ordersWithoutAmt = `CREATE TABLE orders (
    id bigint NOT NULL,
    email text,
    CONSTRAINT orders_pkey PRIMARY KEY (id)
);`

// A desired file drops a column that a publication's column list names,
// and the CHECK that reads it. PostgreSQL refuses to drop the column while
// the publication names it, so the plan is refused before anything runs,
// with the publication named and the way to free the column: the CHECK is
// still there and still enforced, rather than dropped by an earlier step
// of a run that could only fail at the column. A publication that
// publishes the table whole names no column and does not hold the drop
// back. Column lists exist from PostgreSQL 15; before that only the
// whole-table publication is created, and the plan is not refused.
func TestRunDesiredRefusesDroppingAColumnAPublicationNames(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	schema := testutil.NewSchema(t, pool)
	listed, whole := schema+"_listed", schema+"_whole"
	t.Cleanup(func() {
		for _, name := range []string{listed, whole} {
			if _, err := pool.Exec(context.WithoutCancel(t.Context()), "DROP PUBLICATION IF EXISTS "+name); err != nil {
				t.Logf("drop publication %s: %v", name, err)
			}
		}
	})
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.orders (
			id bigint NOT NULL,
			email text,
			amt int,
			CONSTRAINT orders_pkey PRIMARY KEY (id),
			CONSTRAINT orders_amt_check CHECK (amt >= 0)
		);
		INSERT INTO %[1]s.orders VALUES (1, 'a@example.com', 5);
		CREATE PUBLICATION %[2]s FOR TABLE %[1]s.orders`, schema, whole))
	require.NoError(t, err)

	var version int
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT current_setting('server_version_num')::int`).Scan(&version))
	const publicationFiltersSince = 150000
	if version < publicationFiltersSince {
		report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: parseDesired(t, ordersWithoutAmt)})
		require.NoError(t, err)
		for _, ps := range report.Statements {
			for _, d := range ps.Decisions {
				assert.Empty(t, d.DependentPublications, ps.SQL)
			}
		}
		return
	}

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE PUBLICATION %s FOR TABLE %s.orders (id, email, amt)`, listed, schema))
	require.NoError(t, err)

	report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: parseDesired(t, ordersWithoutAmt)})
	require.NoError(t, err)
	var drop *planner.Decision
	for _, ps := range report.Statements {
		for i, d := range ps.Decisions {
			if d.Kind == statement.OpDropColumn {
				drop = &ps.Decisions[i]
			}
		}
	}
	require.NotNil(t, drop, "the plan drops amt")
	assert.Equal(t, planner.RouteRefuse, drop.Route)
	assert.Equal(t, planner.ReasonDependentPublication, drop.Reason)
	assert.Equal(t, []string{`"` + listed + `"`}, drop.DependentPublications)

	res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{
		Schema:              schema,
		Desired:             parseDesired(t, ordersWithoutAmt),
		ExpectedFingerprint: report.Fingerprint,
		AllowDestructive:    schema + ".orders",
	}, runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeRefused, res.Outcome, res.Detail)
	assert.Contains(t, res.Detail, `has no safe path: DROP COLUMN amt changes a column named by publication "`+listed+`"`)
	assert.Contains(t, res.Detail, planner.PublicationHelp(statement.OpDropColumn)+"; nothing was executed")

	var checks int
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_constraint
		WHERE conrelid = (quote_ident($1) || '.orders')::regclass AND conname = 'orders_amt_check'`, schema).Scan(&checks))
	assert.Equal(t, 1, checks, "nothing ran: the CHECK on amt is still there")
	assert.Equal(t, "integer", columnType(t, pool, schema, "orders", "amt"))
}
