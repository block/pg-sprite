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

// requireForeignKeyDrop asserts the statement drops the orders key to users
// as a destructive, checked drop that runs: the drop carries the definition
// it read, so the run refuses a key replaced under the name since.
func requireForeignKeyDrop(t *testing.T, schema string, st plan.Statement) {
	t.Helper()
	assert.Equal(t, schemadiff.ChangeDropForeignKey, st.Kind)
	assert.Equal(t, fmt.Sprintf("ALTER TABLE %s.orders DROP CONSTRAINT orders_user_id_fkey", schema), st.SQL)
	assert.True(t, st.Destructive)
	assert.Equal(t, router.DispositionExecute, st.Disposition)
	assert.Equal(t, []string{st.SQL}, st.ExecSQL)
	assert.Equal(t, &schemadiff.DroppedObject{
		Kind:       schemadiff.DroppedConstraint,
		Name:       "orders_user_id_fkey",
		Definition: "FOREIGN KEY (user_id) REFERENCES users(id)",
	}, st.Drops)
	assert.Empty(t, st.Reason)
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

// A foreign key the desired file adds on a live table runs online: the plan
// executes it as ADD CONSTRAINT ... NOT VALID, which holds its locks only
// briefly, then VALIDATE CONSTRAINT, which checks the existing rows while
// reads and writes continue.
func TestPlanAddForeignKeyRunsAsNotValidThenValidate(t *testing.T) {
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
	assert.Equal(t, router.DispositionExecute, st.Disposition)
	assert.Equal(t, []string{
		fmt.Sprintf("ALTER TABLE %[1]s.orders ADD CONSTRAINT orders_user_id_fkey FOREIGN KEY (user_id) REFERENCES %[1]s.users (id) NOT VALID", schema),
		fmt.Sprintf(`ALTER TABLE "%s"."orders" VALIDATE CONSTRAINT "orders_user_id_fkey"`, schema),
	}, st.ExecSQL)
	assert.Equal(t, &schemadiff.ConstraintValidation{
		Constraint: "orders_user_id_fkey",
		Def:        "FOREIGN KEY (user_id) REFERENCES users(id) NOT VALID",
	}, st.Validates, "the validation step checks the key its add left, so it never validates a key put in its place")
	assert.Equal(t, router.DispositionExecute, report.Disposition)
}

// A live foreign key left NOT VALID with the declared definition plans as one
// validation, not as a drop and re-add, and runs that validation as the one
// statement that converges the table.
func TestPlanNotValidForeignKeyIsValidated(t *testing.T) {
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
	assert.Equal(t, router.DispositionExecute, st.Disposition)
	assert.Equal(t, []string{st.SQL}, st.ExecSQL)
	assert.Equal(t, router.DispositionExecute, report.Disposition)
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
	assert.Equal(t, router.DispositionExecute, report.Statements[0].Disposition)
}

// A live foreign key the desired file no longer declares is planned as a
// destructive drop that runs, as the revert of a change that added it does.
func TestPlanUndeclaredLiveForeignKeyIsDropped(t *testing.T) {
	pool, schema := foreignKeyPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.orders (
			id bigint PRIMARY KEY,
			user_id bigint NOT NULL REFERENCES %[1]s.users (id)
		)`, schema))
	require.NoError(t, err)

	report := planOrders(t, t.Context(), pool, schema, ordersWithoutForeignKey)

	require.Len(t, report.Statements, 1)
	requireForeignKeyDrop(t, schema, report.Statements[0])
	assert.Equal(t, router.DispositionExecute, report.Disposition)
}

// A live foreign key into another schema cannot be declared, since a desired
// file names tables unqualified, so the diff plans its drop whatever the file
// says. The drop is shown for review and refused, so planning an unrelated
// change to the table never removes a key its author had no way to keep.
func TestPlanDropOfAForeignKeyIntoAnotherSchemaIsRefused(t *testing.T) {
	pool, schema := foreignKeyPool(t)
	other := testutil.NewSchema(t, pool)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[2]s.regions (id bigint PRIMARY KEY);
		CREATE TABLE %[1]s.orders (
			id bigint PRIMARY KEY,
			user_id bigint NOT NULL,
			region_id bigint CONSTRAINT orders_region_id_fkey REFERENCES %[2]s.regions (id)
		)`, schema, other))
	require.NoError(t, err)

	report := planOrders(t, t.Context(), pool, schema, `
CREATE TABLE orders (
    id bigint PRIMARY KEY,
    user_id bigint NOT NULL,
    region_id bigint
);`)

	require.Len(t, report.Statements, 1)
	st := report.Statements[0]
	assert.Equal(t, schemadiff.ChangeDropForeignKey, st.Kind)
	assert.Equal(t, fmt.Sprintf("ALTER TABLE %s.orders DROP CONSTRAINT orders_region_id_fkey", schema), st.SQL)
	assert.True(t, st.Destructive)
	assert.Equal(t, router.DispositionRefuse, st.Disposition)
	assert.Equal(t, verdict.ReasonDeclarativeForeignKey, st.Reason)
	assert.Equal(t, verdict.CauseUndeclarableForeignKey, st.RefusalCause)
	assert.Empty(t, st.ExecSQL)
	assert.Equal(t, router.DispositionRefuse, report.Disposition)
}

// A live foreign key from a table to itself whose referenced column is
// unique only through a standalone unique index cannot be declared either:
// the file's CREATE TABLE would declare the key before the index exists. A
// file that keeps the index and adds an unrelated column still plans the
// key's drop, and the drop is shown for review and refused, so the change
// never removes a key its author had no way to keep.
func TestPlanDropOfASelfReferencingForeignKeyOnAStandaloneIndexIsRefused(t *testing.T) {
	pool, schema := foreignKeyPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.orders (
			id bigint PRIMARY KEY,
			code text NOT NULL,
			parent_code text
		);
		CREATE UNIQUE INDEX orders_code_idx ON %[1]s.orders (code);
		ALTER TABLE %[1]s.orders ADD CONSTRAINT orders_parent_fkey FOREIGN KEY (parent_code) REFERENCES %[1]s.orders (code)`, schema))
	require.NoError(t, err)

	report := planOrders(t, t.Context(), pool, schema, `
CREATE TABLE orders (
    id bigint PRIMARY KEY,
    code text NOT NULL,
    parent_code text,
    note text
);
CREATE UNIQUE INDEX orders_code_idx ON orders (code);`)

	var drop *plan.Statement
	for i, st := range report.Statements {
		if st.Kind == schemadiff.ChangeDropForeignKey {
			drop = &report.Statements[i]
		}
	}
	require.NotNil(t, drop, "the diff plans the drop of the key the file cannot declare")
	assert.Equal(t, fmt.Sprintf("ALTER TABLE %s.orders DROP CONSTRAINT orders_parent_fkey", schema), drop.SQL)
	assert.Equal(t, router.DispositionRefuse, drop.Disposition)
	assert.Equal(t, verdict.ReasonDeclarativeForeignKey, drop.Reason)
	assert.Equal(t, verdict.CauseUndeclarableForeignKey, drop.RefusalCause)
	assert.Empty(t, drop.ExecSQL)
	assert.Equal(t, router.DispositionRefuse, report.Disposition)
}

// A foreign key whose definition changes under the same name is replaced by
// one statement that drops the live key and adds the new one NOT VALID, then
// validated on its own. Every write is checked by one key or the other at
// every moment, so nothing is refused; the statement is destructive, naming
// the key it drops, so the plan is reviewed as one that drops a key.
func TestPlanChangedForeignKeyIsReplacedInOneStatement(t *testing.T) {
	pool, schema := foreignKeyPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.orders (
			id bigint PRIMARY KEY,
			user_id bigint NOT NULL REFERENCES %[1]s.users (id)
		)`, schema))
	require.NoError(t, err)

	report := planOrders(t, t.Context(), pool, schema, `
CREATE TABLE orders (
    id bigint PRIMARY KEY,
    user_id bigint NOT NULL REFERENCES users (id) ON DELETE CASCADE
);`)

	require.Len(t, report.Statements, 2)
	replace := report.Statements[0]
	assert.Equal(t, schemadiff.ChangeAddForeignKey, replace.Kind)
	assert.Equal(t, fmt.Sprintf("ALTER TABLE %[1]s.orders DROP CONSTRAINT orders_user_id_fkey, ADD CONSTRAINT orders_user_id_fkey FOREIGN KEY (user_id) REFERENCES %[1]s.users (id) ON DELETE CASCADE NOT VALID", schema), replace.SQL)
	assert.Equal(t, router.DispositionExecute, replace.Disposition)
	assert.True(t, replace.Destructive, "the statement drops the live key")
	require.NotNil(t, replace.Drops)
	assert.Equal(t, "orders_user_id_fkey", replace.Drops.Name)
	validate := report.Statements[1]
	assert.Equal(t, schemadiff.ChangeValidateForeignKey, validate.Kind)
	assert.Equal(t, fmt.Sprintf("ALTER TABLE %s.orders VALIDATE CONSTRAINT orders_user_id_fkey", schema), validate.SQL)
	assert.Equal(t, router.DispositionExecute, validate.Disposition)
	assert.Equal(t, router.DispositionExecute, report.Disposition)
}

// A foreign key changed under the same name in a plan that drops a column
// is replaced in one statement before the column is dropped, so every write
// is checked against a key throughout.
func TestPlanChangedForeignKeyBesideAColumnDropIsReplacedFirst(t *testing.T) {
	pool, schema := foreignKeyPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.orders (
			id bigint PRIMARY KEY,
			user_id bigint NOT NULL REFERENCES %[1]s.users (id),
			legacy text
		)`, schema))
	require.NoError(t, err)

	report := planOrders(t, t.Context(), pool, schema, `
CREATE TABLE orders (
    id bigint PRIMARY KEY,
    user_id bigint NOT NULL REFERENCES users (id) ON DELETE CASCADE
);`)

	require.Len(t, report.Statements, 3)
	assert.Equal(t, fmt.Sprintf("ALTER TABLE %[1]s.orders DROP CONSTRAINT orders_user_id_fkey, ADD CONSTRAINT orders_user_id_fkey FOREIGN KEY (user_id) REFERENCES %[1]s.users (id) ON DELETE CASCADE NOT VALID", schema), report.Statements[0].SQL)
	assert.Equal(t, fmt.Sprintf("ALTER TABLE %s.orders VALIDATE CONSTRAINT orders_user_id_fkey", schema), report.Statements[1].SQL)
	assert.Equal(t, fmt.Sprintf("ALTER TABLE %s.orders DROP legacy", schema), report.Statements[2].SQL)
	for _, st := range report.Statements {
		assert.Equal(t, router.DispositionExecute, st.Disposition, st.SQL)
	}
	assert.Equal(t, router.DispositionExecute, report.Disposition)
}

// A foreign key changed under the same name in a plan that drops a column
// beside an added column that may rewrite the table cannot share one
// statement with its replacement: the column drop runs first, so the key's
// drop would leave writes checked against no key until the new one is added.
// The drop is shown for review and refused with the cause that names that
// kind of drop, so the remedy offered is the column change on its own first:
// a new name for the key would still be dropped first.
func TestPlanChangedForeignKeyBesideAColumnDropAndRewritingAddIsRefused(t *testing.T) {
	pool, schema := foreignKeyPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.orders (
			id bigint PRIMARY KEY,
			user_id bigint NOT NULL REFERENCES %[1]s.users (id),
			legacy text
		)`, schema))
	require.NoError(t, err)

	report := planOrders(t, t.Context(), pool, schema, `
CREATE TABLE orders (
    id bigint PRIMARY KEY,
    user_id bigint NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    placed_at timestamptz NOT NULL DEFAULT clock_timestamp()
);`)

	var drop *plan.Statement
	for i, st := range report.Statements {
		if st.Kind == schemadiff.ChangeDropForeignKey {
			drop = &report.Statements[i]
		}
	}
	require.NotNil(t, drop, "the key's drop is its own statement beside the column drop")
	assert.Equal(t, fmt.Sprintf("ALTER TABLE %s.orders DROP CONSTRAINT orders_user_id_fkey", schema), drop.SQL)
	assert.Equal(t, router.DispositionRefuse, drop.Disposition)
	assert.Equal(t, verdict.ReasonDeclarativeForeignKey, drop.Reason)
	assert.Equal(t, verdict.CauseColumnFirstForeignKey, drop.RefusalCause)
	assert.Empty(t, drop.ExecSQL)
	assert.Equal(t, router.DispositionRefuse, report.Disposition)
}

// A foreign key the file declares changed under a new name is added and
// validated before the old key is dropped, so every write is checked
// against a key throughout, and a validation that fails leaves the old key
// standing.
func TestPlanForeignKeyMovedToANewNameIsAddedBeforeTheOldOneIsDropped(t *testing.T) {
	pool, schema := foreignKeyPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.orders (
			id bigint PRIMARY KEY,
			user_id bigint NOT NULL REFERENCES %[1]s.users (id)
		)`, schema))
	require.NoError(t, err)

	report := planOrders(t, t.Context(), pool, schema, `
CREATE TABLE orders (
    id bigint PRIMARY KEY,
    user_id bigint NOT NULL CONSTRAINT orders_user_id_cascade_fkey REFERENCES users (id) ON DELETE CASCADE
);`)

	require.Len(t, report.Statements, 2)
	add := report.Statements[0]
	assert.Equal(t, schemadiff.ChangeAddForeignKey, add.Kind)
	assert.Equal(t, router.DispositionExecute, add.Disposition)
	assert.Equal(t, []string{
		fmt.Sprintf("ALTER TABLE %[1]s.orders ADD CONSTRAINT orders_user_id_cascade_fkey FOREIGN KEY (user_id) REFERENCES %[1]s.users (id) ON DELETE CASCADE NOT VALID", schema),
		fmt.Sprintf(`ALTER TABLE "%s"."orders" VALIDATE CONSTRAINT "orders_user_id_cascade_fkey"`, schema),
	}, add.ExecSQL)
	requireForeignKeyDrop(t, schema, report.Statements[1])
	assert.Equal(t, router.DispositionExecute, report.Disposition)
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
	schema, err := planOrdersForeignKeyAsRole(t, "")
	require.ErrorIs(t, err, schemadiff.ErrReferencedTableNotReadable)
	assert.ErrorContains(t, err, schema+".users")
}

// A planning role that can read a referenced table but lacks REFERENCES on
// it cannot plan its foreign key either: the run would fail creating the key
// on the live table, so the plan refuses with a typed error naming the table
// instead of reporting a plan the role cannot execute.
func TestPlanForeignKeyWithoutReferencesGrantFails(t *testing.T) {
	schema, err := planOrdersForeignKeyAsRole(t, "SELECT")
	require.ErrorIs(t, err, schemadiff.ErrReferencedTableNotReferenceable)
	assert.ErrorContains(t, err, schema+".users(id)")
}

// PostgreSQL lets a role add a foreign key when it holds REFERENCES on each
// referenced column, without the privilege on the whole table, so planning
// accepts a column grant too.
func TestPlanForeignKeyWithColumnReferencesGrantPlans(t *testing.T) {
	_, report, err := planOrdersAsRole(t, "SELECT, REFERENCES (id)", false, ordersReferencingUsers)
	require.NoError(t, err)
	require.Len(t, report.Statements, 1)
	assert.Equal(t, schemadiff.ChangeAddForeignKey, report.Statements[0].Kind)
}

// A plan that leaves a live foreign key alone needs no REFERENCES on the
// table it references: a role that can only read users plans a new column on
// orders, whose key to users the file keeps unchanged.
func TestPlanUnchangedForeignKeyNeedsNoReferencesGrant(t *testing.T) {
	_, report, err := planOrdersAsRole(t, "SELECT", true, `
CREATE TABLE orders (
    id bigint PRIMARY KEY,
    user_id bigint NOT NULL REFERENCES users (id),
    note text
);`)
	require.NoError(t, err)
	require.Len(t, report.Statements, 1)
	assert.Equal(t, schemadiff.ChangeAddColumn, report.Statements[0].Kind)
	assert.Contains(t, report.Statements[0].SQL, "note")
}

// ordersReferencingUsers is a desired orders file whose key to users the
// live orders table does not have.
const ordersReferencingUsers = `
CREATE TABLE orders (
    id bigint PRIMARY KEY,
    user_id bigint NOT NULL REFERENCES users (id)
);`

// planOrdersForeignKeyAsRole plans a foreign key from orders to users as a
// throwaway role that holds the given privileges on users ("" for none) and
// returns the test schema and the plan's error.
func planOrdersForeignKeyAsRole(t *testing.T, usersGrant string) (string, error) {
	t.Helper()
	schema, _, err := planOrdersAsRole(t, usersGrant, false, ordersReferencingUsers)
	return schema, err
}

// planOrdersAsRole plans the desired orders file as a throwaway role that
// can read orders and holds the given privileges on users ("" for none). The
// live orders table carries a key to users when liveKey is set. It returns
// the test schema, the plan, and the plan's error.
func planOrdersAsRole(t *testing.T, usersGrant string, liveKey bool, desired string) (string, plan.Report, error) {
	t.Helper()
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
	if liveKey {
		_, err = pool.Exec(t.Context(), fmt.Sprintf(
			"ALTER TABLE %[1]s.orders ADD CONSTRAINT orders_user_id_fkey FOREIGN KEY (user_id) REFERENCES %[1]s.users (id)", schema))
		require.NoError(t, err)
	}
	if usersGrant != "" {
		_, err = pool.Exec(t.Context(), fmt.Sprintf("GRANT %s ON %s.users TO %s",
			usersGrant, schema, pgx.Identifier{role}.Sanitize()))
		require.NoError(t, err)
	}
	var db string
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT current_database()").Scan(&db))
	_, err = pool.Exec(t.Context(), fmt.Sprintf("GRANT CREATE ON DATABASE %s TO %s",
		pgx.Identifier{db}.Sanitize(), pgx.Identifier{role}.Sanitize()))
	require.NoError(t, err)
	t.Cleanup(func() {
		// Revoke every grant so the role's own cleanup can drop it.
		_, err := pool.Exec(context.WithoutCancel(t.Context()), fmt.Sprintf(`
			REVOKE ALL ON %[1]s.orders FROM %[2]s;
			REVOKE ALL ON %[1]s.users FROM %[2]s;
			REVOKE ALL ON SCHEMA %[1]s FROM %[2]s;
			REVOKE CREATE ON DATABASE %[3]s FROM %[2]s`,
			schema, pgx.Identifier{role}.Sanitize(), pgx.Identifier{db}.Sanitize()))
		assert.NoError(t, err)
	})

	planner, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: urlAsRole(t, url, role)})
	require.NoError(t, err)
	t.Cleanup(planner.Close)

	report, err := diffplan.Plan(t.Context(), planner, diffplan.Request{Schema: schema, Desired: parseDesired(t, desired)})
	return schema, report, err
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

// A new table that declares a foreign key is created with it: the table is
// empty, so there are no rows to check, and the create step takes its lock on
// the referenced table under the same brief budget as every create step.
func TestPlanGreenfieldTableWithForeignKeyIsCreated(t *testing.T) {
	pool, schema := foreignKeyPool(t)

	report := planOrders(t, t.Context(), pool, schema, ordersWithForeignKey)

	require.NotNil(t, report.TableExists)
	assert.False(t, *report.TableExists)
	require.Len(t, report.Statements, 1)
	st := report.Statements[0]
	assert.Equal(t, schemadiff.ChangeCreateTable, st.Kind)
	assert.Contains(t, st.SQL, "REFERENCES "+schema+".users (id)", "the reference binds to the target schema")
	assert.Empty(t, st.Cause)
	assert.Equal(t, router.DispositionExecute, st.Disposition)
	assert.Equal(t, router.DispositionExecute, report.Disposition)
}

// A foreign-key drop beside another change routes like the other change, so
// the whole plan executes.
func TestPlanForeignKeyDropBesideAnotherChangeExecutes(t *testing.T) {
	pool, schema := foreignKeyPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.orders (
			id bigint PRIMARY KEY,
			user_id bigint NOT NULL REFERENCES %[1]s.users (id)
		)`, schema))
	require.NoError(t, err)

	report := planOrders(t, t.Context(), pool, schema, `
CREATE TABLE orders (
    id bigint PRIMARY KEY,
    user_id bigint NOT NULL,
    note text
);`)

	kinds := map[schemadiff.ChangeKind]plan.Statement{}
	for _, st := range report.Statements {
		kinds[st.Kind] = st
	}
	require.Contains(t, kinds, schemadiff.ChangeAddColumn)
	require.Contains(t, kinds, schemadiff.ChangeDropForeignKey)
	assert.Equal(t, router.DispositionExecute, kinds[schemadiff.ChangeAddColumn].Disposition)
	requireForeignKeyDrop(t, schema, kinds[schemadiff.ChangeDropForeignKey])
	assert.Equal(t, router.DispositionExecute, report.Disposition)
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
