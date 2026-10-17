package diffplan_test

import (
	"context"
	"fmt"
	neturl "net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/verdict"
)

// foreignKeyPool returns a pool and a fresh schema holding a users table
// that orders reference.
func foreignKeyPool(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %s.users (
			id bigint PRIMARY KEY,
			email text NOT NULL
		)`, schema))
	require.NoError(t, err)
	return pool, schema
}

// planOrders plans the desired orders file against the schema.
func planOrders(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema, desired string) plan.Report {
	t.Helper()
	report, err := diffplan.Plan(ctx, pool, diffplan.Request{Schema: schema, Desired: parseDesired(t, desired)})
	require.NoError(t, err)
	return report
}

// requireForeignKeyRefusal asserts the statement is planned and refused with
// the declarative foreign-key reason, so the diff is reviewable but never
// executes.
func requireForeignKeyRefusal(t *testing.T, st plan.Statement) {
	t.Helper()
	assert.Equal(t, router.DispositionRefuse, st.Disposition)
	assert.Equal(t, verdict.ReasonDeclarativeForeignKey, st.Reason)
	assert.Equal(t, verdict.ClassCapabilityBoundary, st.Class)
	assert.Empty(t, st.Backend)
	assert.Empty(t, st.ExecSQL)
}

const ordersWithForeignKey = `
CREATE TABLE orders (
    id bigint PRIMARY KEY,
    user_id bigint NOT NULL REFERENCES users (id)
);`

const ordersWithoutForeignKey = `
CREATE TABLE orders (
    id bigint PRIMARY KEY,
    user_id bigint NOT NULL
);`

// A table whose desired file declares the foreign key it already has plans
// no change: the scratch build binds the reference to a stand-in named like
// the live table, so both sides print the same constraint definition.
func TestPlanForeignKeyMatchingLivePlansNothing(t *testing.T) {
	pool, schema := foreignKeyPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.orders (
			id bigint PRIMARY KEY,
			user_id bigint NOT NULL REFERENCES %[1]s.users (id)
		)`, schema))
	require.NoError(t, err)

	report := planOrders(t, t.Context(), pool, schema, ordersWithForeignKey)
	assert.Empty(t, report.Statements)
	assert.Equal(t, router.DispositionExecute, report.Disposition)
}

// A self-referencing foreign key binds to the desired table itself and needs
// no stand-in.
func TestPlanSelfReferencingForeignKeyMatchingLivePlansNothing(t *testing.T) {
	pool, schema := foreignKeyPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.employees (
			id bigint PRIMARY KEY,
			manager_id bigint REFERENCES %[1]s.employees (id)
		)`, schema))
	require.NoError(t, err)

	report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: parseDesired(t, `
CREATE TABLE employees (
    id bigint PRIMARY KEY,
    manager_id bigint REFERENCES employees (id)
);`)})
	require.NoError(t, err)
	assert.Empty(t, report.Statements)
}

// A foreign key the desired file adds is planned so the operator sees the
// diff, and refused for execution with the declarative foreign-key reason.
func TestPlanAddForeignKeyIsShownAndRefused(t *testing.T) {
	pool, schema := foreignKeyPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %s.orders (
			id bigint PRIMARY KEY,
			user_id bigint NOT NULL
		)`, schema))
	require.NoError(t, err)

	report := planOrders(t, t.Context(), pool, schema, ordersWithForeignKey)

	require.Len(t, report.Statements, 1)
	st := report.Statements[0]
	assert.Equal(t, schemadiff.ChangeAddForeignKey, st.Kind)
	assert.Equal(t, fmt.Sprintf(
		"ALTER TABLE %[1]s.orders ADD CONSTRAINT orders_user_id_fkey FOREIGN KEY (user_id) REFERENCES %[1]s.users (id)", schema),
		st.SQL)
	assert.False(t, st.Destructive)
	requireForeignKeyRefusal(t, st)
	assert.Equal(t, router.DispositionRefuse, report.Disposition)
	assert.Equal(t, verdict.ReasonDeclarativeForeignKey, report.Reason)
}

// A live foreign key left NOT VALID with the declared definition plans as one
// validation, not as a drop and re-add, and is refused like every declarative
// foreign-key change: the plan shows the one statement that converges the
// table, and nothing runs it.
func TestPlanNotValidForeignKeyIsValidatedAndRefused(t *testing.T) {
	pool, schema := foreignKeyPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.orders (
			id bigint PRIMARY KEY,
			user_id bigint NOT NULL
		);
		ALTER TABLE %[1]s.orders ADD CONSTRAINT orders_user_id_fkey
			FOREIGN KEY (user_id) REFERENCES %[1]s.users (id) NOT VALID`, schema))
	require.NoError(t, err)

	report := planOrders(t, t.Context(), pool, schema, ordersWithForeignKey)

	require.Len(t, report.Statements, 1)
	st := report.Statements[0]
	assert.Equal(t, schemadiff.ChangeValidateForeignKey, st.Kind)
	assert.Equal(t, fmt.Sprintf("ALTER TABLE %s.orders VALIDATE CONSTRAINT orders_user_id_fkey", schema), st.SQL)
	assert.False(t, st.Destructive)
	requireForeignKeyRefusal(t, st)
	assert.Equal(t, router.DispositionRefuse, report.Disposition)
	assert.Equal(t, verdict.ReasonDeclarativeForeignKey, report.Reason)
}

// Planning a foreign key never locks the live referenced table, even in the
// public schema, where an unqualified reference in the scratch build would
// otherwise resolve to the live table through the search_path. A writer
// holds ROW EXCLUSIVE on the referenced table in an open transaction; that
// conflicts with the SHARE ROW EXCLUSIVE a foreign key takes, so a scratch
// build bound to the live table would wait on it and the plan would time
// out.
func TestPlanForeignKeyNeverLocksLiveReferencedTable(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	// The throwaway schema name makes the public table names unique.
	suffix := testutil.NewSchema(t, pool)
	users, orders := "users_"+suffix, "orders_"+suffix
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE public.%[1]s (
			id bigint PRIMARY KEY
		);
		CREATE TABLE public.%[2]s (
			id bigint PRIMARY KEY,
			user_id bigint NOT NULL
		)`, users, orders))
	require.NoError(t, err)
	t.Cleanup(func() {
		// t.Context is cancelled by cleanup time; strip the cancellation.
		_, err := pool.Exec(context.WithoutCancel(t.Context()), fmt.Sprintf("DROP TABLE IF EXISTS public.%s, public.%s", orders, users))
		assert.NoError(t, err)
	})

	writer, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, writer.Rollback(context.WithoutCancel(t.Context()))) })
	_, err = writer.Exec(t.Context(), fmt.Sprintf("INSERT INTO public.%s (id) VALUES (1)", users))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	report := planOrders(t, ctx, pool, "public", fmt.Sprintf(`
CREATE TABLE %[2]s (
    id bigint PRIMARY KEY,
    user_id bigint NOT NULL REFERENCES %[1]s (id)
);`, users, orders))

	require.Len(t, report.Statements, 1)
	assert.Equal(t, schemadiff.ChangeAddForeignKey, report.Statements[0].Kind)
	requireForeignKeyRefusal(t, report.Statements[0])
}

// A live foreign key the desired file no longer declares is planned as a
// destructive drop and refused with the declarative foreign-key reason.
func TestPlanUndeclaredLiveForeignKeyIsShownAndRefused(t *testing.T) {
	pool, schema := foreignKeyPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.orders (
			id bigint PRIMARY KEY,
			user_id bigint NOT NULL REFERENCES %[1]s.users (id)
		)`, schema))
	require.NoError(t, err)

	report := planOrders(t, t.Context(), pool, schema, ordersWithoutForeignKey)

	require.Len(t, report.Statements, 1)
	st := report.Statements[0]
	assert.Equal(t, schemadiff.ChangeDropForeignKey, st.Kind)
	assert.Equal(t, fmt.Sprintf("ALTER TABLE %s.orders DROP CONSTRAINT orders_user_id_fkey", schema), st.SQL)
	assert.True(t, st.Destructive)
	requireForeignKeyRefusal(t, st)
}

// A foreign key whose referenced table does not exist cannot be planned: the
// scratch build has no live table to copy the referenced key from.
func TestPlanForeignKeyToMissingTableFails(t *testing.T) {
	pool, schema := foreignKeyPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %s.orders (
			id bigint PRIMARY KEY,
			user_id bigint NOT NULL
		)`, schema))
	require.NoError(t, err)

	_, err = diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: parseDesired(t, `
CREATE TABLE orders (
    id bigint PRIMARY KEY,
    user_id bigint NOT NULL REFERENCES customers (id)
);`)})
	require.ErrorIs(t, err, schemadiff.ErrReferencedTableNotFound)
	assert.ErrorContains(t, err, schema+".customers")
}

// A planning role that cannot read a referenced table cannot plan its
// foreign key: the scratch build copies the table's shape with LIKE, which
// needs SELECT on it. The plan fails with a typed error naming the table.
func TestPlanForeignKeyToUnreadableTableFails(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	role := testutil.NewRole(t, pool, "LOGIN PASSWORD '"+plannerPassword+"'")
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.users (
			id bigint PRIMARY KEY
		);
		CREATE TABLE %[1]s.orders (
			id bigint PRIMARY KEY,
			user_id bigint NOT NULL
		);
		GRANT USAGE, CREATE ON SCHEMA %[1]s TO %[2]s;
		GRANT SELECT ON %[1]s.orders TO %[2]s`, schema, pgx.Identifier{role}.Sanitize()))
	require.NoError(t, err)
	var db string
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT current_database()").Scan(&db))
	_, err = pool.Exec(t.Context(), fmt.Sprintf("GRANT CREATE ON DATABASE %s TO %s",
		pgx.Identifier{db}.Sanitize(), pgx.Identifier{role}.Sanitize()))
	require.NoError(t, err)
	t.Cleanup(func() {
		// Revoke every grant so the role's own cleanup can drop it.
		_, err := pool.Exec(context.WithoutCancel(t.Context()), fmt.Sprintf(`
			REVOKE ALL ON %[1]s.orders FROM %[2]s;
			REVOKE ALL ON SCHEMA %[1]s FROM %[2]s;
			REVOKE CREATE ON DATABASE %[3]s FROM %[2]s`,
			schema, pgx.Identifier{role}.Sanitize(), pgx.Identifier{db}.Sanitize()))
		assert.NoError(t, err)
	})

	planner, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: urlAsRole(t, url, role)})
	require.NoError(t, err)
	t.Cleanup(planner.Close)

	_, err = diffplan.Plan(t.Context(), planner, diffplan.Request{Schema: schema, Desired: parseDesired(t, `
CREATE TABLE orders (
    id bigint PRIMARY KEY,
    user_id bigint NOT NULL REFERENCES users (id)
);`)})
	require.ErrorIs(t, err, schemadiff.ErrReferencedTableNotReadable)
	assert.ErrorContains(t, err, schema+".users")
}

// plannerPassword is the password the throwaway planning role logs in with.
const plannerPassword = "diffplan-test-password"

// urlAsRole returns serverURL with role, logging in with plannerPassword, as
// the user.
func urlAsRole(t *testing.T, serverURL, role string) string {
	t.Helper()
	u, err := neturl.Parse(serverURL)
	require.NoError(t, err)
	u.User = neturl.UserPassword(role, plannerPassword)
	return u.String()
}

// A new table that declares a foreign key is planned, and its CREATE TABLE is
// refused by the create path with the foreign-key cause.
func TestPlanGreenfieldTableWithForeignKeyIsShownAndRefused(t *testing.T) {
	pool, schema := foreignKeyPool(t)

	report := planOrders(t, t.Context(), pool, schema, ordersWithForeignKey)

	require.NotNil(t, report.TableExists)
	assert.False(t, *report.TableExists)
	require.Len(t, report.Statements, 1)
	st := report.Statements[0]
	assert.Equal(t, schemadiff.ChangeCreateTable, st.Kind)
	assert.Contains(t, st.SQL, "REFERENCES users (id)")
	assert.Equal(t, executor.CreateShapeForeignKey, st.Cause)
	requireForeignKeyRefusal(t, st)
}

// Statements without a foreign key in the same plan still execute; only the
// foreign-key statement is refused, and admission is all-or-nothing.
func TestPlanForeignKeyRefusalLeavesOtherStatementsRouted(t *testing.T) {
	pool, schema := foreignKeyPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %s.orders (
			id bigint PRIMARY KEY,
			user_id bigint NOT NULL
		)`, schema))
	require.NoError(t, err)

	report := planOrders(t, t.Context(), pool, schema, `
CREATE TABLE orders (
    id bigint PRIMARY KEY,
    user_id bigint NOT NULL REFERENCES users (id),
    note text
);`)

	kinds := map[schemadiff.ChangeKind]plan.Statement{}
	for _, st := range report.Statements {
		kinds[st.Kind] = st
	}
	require.Contains(t, kinds, schemadiff.ChangeAddColumn)
	require.Contains(t, kinds, schemadiff.ChangeAddForeignKey)
	assert.Equal(t, router.DispositionExecute, kinds[schemadiff.ChangeAddColumn].Disposition)
	requireForeignKeyRefusal(t, kinds[schemadiff.ChangeAddForeignKey])
	assert.Equal(t, router.DispositionRefuse, report.Disposition)
}

// A stand-in's indexes never take a name the desired file uses. LIKE ...
// INCLUDING INDEXES names the copied indexes after the stand-in table, so
// a live users table whose primary key is users_pk gets a stand-in index
// named users_pkey; the desired orders file owns an index by that name, and
// the unchanged schema still plans no change. The renamed stand-in indexes
// carry the scratch schema's random name, so no fixed internal name is
// reserved either: an index with a pg-sprite-looking name plans as usual.
func TestPlanForeignKeyStandInIndexNamesNeverCollide(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.users (
			id bigint CONSTRAINT users_pk PRIMARY KEY
		);
		CREATE TABLE %[1]s.orders (
			id bigint PRIMARY KEY,
			user_id bigint NOT NULL REFERENCES %[1]s.users (id)
		);
		CREATE INDEX users_pkey ON %[1]s.orders (user_id);
		CREATE INDEX pgsprite_standin_index_0 ON %[1]s.orders (id, user_id)`, schema))
	require.NoError(t, err)

	report := planOrders(t, t.Context(), pool, schema, `
CREATE TABLE orders (
    id bigint PRIMARY KEY,
    user_id bigint NOT NULL REFERENCES users (id)
);
CREATE INDEX users_pkey ON orders (user_id);
CREATE INDEX pgsprite_standin_index_0 ON orders (id, user_id);`)
	assert.Empty(t, report.Statements)
}
