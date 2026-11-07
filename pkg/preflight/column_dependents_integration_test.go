package preflight_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
)

// ReadColumnDependents maps each column to the views and materialized views
// that read it, from any schema. A view that reads the table without naming
// a column, or reads another table, does not count against the column, and
// a column no view reads is absent. A view on an inheritance child counts
// for the tree, not for the table alone.
func TestReadColumnDependentsViews(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	schema := testutil.NewSchema(t, pool)
	other := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.users (id bigint PRIMARY KEY, email text, note text, unread text);
		CREATE TABLE %[1]s.teams (id bigint PRIMARY KEY, email text);
		CREATE VIEW %[1]s.contacts AS SELECT id, email FROM %[1]s.users;
		CREATE MATERIALIZED VIEW %[2]s."Notes" AS SELECT note FROM %[1]s.users WHERE email IS NOT NULL;
		CREATE VIEW %[1]s.user_count AS SELECT pg_catalog.count(*) FROM %[1]s.users;
		CREATE VIEW %[1]s.team_mail AS SELECT email FROM %[1]s.teams;
		CREATE TABLE %[1]s.admins (level int) INHERITS (%[1]s.users);
		CREATE VIEW %[1]s.admin_notes AS SELECT note, level FROM %[1]s.admins`, schema, other))
	require.NoError(t, err)

	deps, err := preflight.ReadColumnDependents(t.Context(), pool, schema, "users")
	require.NoError(t, err)
	views := deps.Views
	assert.Equal(t, map[string][]string{
		"id":    {fmt.Sprintf(`"%s"."contacts"`, schema)},
		"email": sortedQualified([2]string{schema, "contacts"}, [2]string{other, "Notes"}),
		"note":  {fmt.Sprintf(`"%s"."Notes"`, other)},
	}, views.Own)
	assert.Equal(t, map[string][]string{
		"id":    {fmt.Sprintf(`"%s"."contacts"`, schema)},
		"email": sortedQualified([2]string{schema, "contacts"}, [2]string{other, "Notes"}),
		"note":  sortedQualified([2]string{schema, "admin_notes"}, [2]string{other, "Notes"}),
	}, views.Tree)
	assert.Equal(t, views.Tree, views.Drop, "admins inherits every column from users alone, so a drop reaches it")
	assert.Empty(t, deps.Policies.Tree, "no policy reads users")
}

// A recursive DROP COLUMN drops a child's copy only when the child does
// not declare the column and every parent it inherits the column from
// drops its own, as both parents in a diamond do, and descends only
// through the copies it drops; a type change reaches every copy. The server is the oracle: with the views Drop names removed the
// drop succeeds, so no other view blocks it, and each view Drop names
// blocks it on its own.
func TestReadColumnDependentsViewsForADrop(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.users (id bigint PRIMARY KEY, email text);
		CREATE TABLE %[1]s.inherited () INHERITS (%[1]s.users);
		CREATE TABLE %[1]s.below_inherited () INHERITS (%[1]s.inherited);
		CREATE TABLE %[1]s.declared (email text) INHERITS (%[1]s.users);
		CREATE TABLE %[1]s.below_declared () INHERITS (%[1]s.declared);
		CREATE TABLE %[1]s.other_parent (email text);
		CREATE TABLE %[1]s.two_parents () INHERITS (%[1]s.users, %[1]s.other_parent);
		CREATE TABLE %[1]s.attached (id bigint NOT NULL, email text);
		ALTER TABLE %[1]s.attached INHERIT %[1]s.users;
		CREATE TABLE %[1]s.branch_a () INHERITS (%[1]s.users);
		CREATE TABLE %[1]s.branch_b () INHERITS (%[1]s.users);
		CREATE TABLE %[1]s.diamond () INHERITS (%[1]s.branch_a, %[1]s.branch_b);
		CREATE TABLE %[1]s.uneven () INHERITS (%[1]s.below_inherited, %[1]s.branch_b);
		CREATE VIEW %[1]s.v_users AS SELECT email FROM ONLY %[1]s.users;
		CREATE VIEW %[1]s.v_inherited AS SELECT email FROM ONLY %[1]s.inherited;
		CREATE VIEW %[1]s.v_below_inherited AS SELECT email FROM %[1]s.below_inherited;
		CREATE VIEW %[1]s.v_declared AS SELECT email FROM ONLY %[1]s.declared;
		CREATE VIEW %[1]s.v_below_declared AS SELECT email FROM %[1]s.below_declared;
		CREATE VIEW %[1]s.v_two_parents AS SELECT email FROM %[1]s.two_parents;
		CREATE VIEW %[1]s.v_attached AS SELECT email FROM %[1]s.attached;
		CREATE VIEW %[1]s.v_diamond AS SELECT email FROM %[1]s.diamond;
		CREATE VIEW %[1]s.v_uneven AS SELECT email FROM %[1]s.uneven;
		CREATE TABLE %[1]s.events (id bigint, email text) PARTITION BY RANGE (id);
		CREATE TABLE %[1]s.events_1 PARTITION OF %[1]s.events FOR VALUES FROM (0) TO (10);
		CREATE VIEW %[1]s.v_events_1 AS SELECT email FROM %[1]s.events_1`, schema))
	require.NoError(t, err)
	qualified := func(names ...string) []string {
		out := make([]string, len(names))
		for i, name := range names {
			out[i] = fmt.Sprintf(`"%s"."%s"`, schema, name)
		}
		return out
	}

	deps, err := preflight.ReadColumnDependents(t.Context(), pool, schema, "users")
	require.NoError(t, err)
	views := deps.Views
	assert.Equal(t, qualified("v_attached", "v_below_declared", "v_below_inherited", "v_declared",
		"v_diamond", "v_inherited", "v_two_parents", "v_uneven", "v_users"), views.Tree["email"])
	assert.Equal(t, qualified("v_below_inherited", "v_diamond", "v_inherited", "v_uneven", "v_users"), views.Drop["email"])
	assert.Equal(t, qualified("v_users"), views.Own["email"])

	dropFails := func(t *testing.T, keep string) error {
		t.Helper()
		tx, err := pool.Begin(t.Context())
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
		for _, view := range views.Drop["email"] {
			if view != keep {
				_, err := tx.Exec(t.Context(), "DROP VIEW "+view)
				require.NoError(t, err)
			}
		}
		_, err = tx.Exec(t.Context(), fmt.Sprintf("ALTER TABLE %s.users DROP COLUMN email", schema))
		return err
	}
	require.NoError(t, dropFails(t, ""), "no view outside Drop blocks the drop")
	for _, view := range views.Drop["email"] {
		var pgErr *pgconn.PgError
		require.ErrorAs(t, dropFails(t, view), &pgErr, view)
		assert.Equal(t, "2BP01", pgErr.Code, view)
	}

	partitioned, err := preflight.ReadColumnDependents(t.Context(), pool, schema, "events")
	require.NoError(t, err)
	assert.Equal(t, qualified("v_events_1"), partitioned.Views.Drop["email"], "a partition never declares a column")
}

// sortedQualified renders the views as ReadColumnDependents orders them: by
// schema, then name, bytewise.
func sortedQualified(views ...[2]string) []string {
	if views[0][0] > views[1][0] {
		views[0], views[1] = views[1], views[0]
	}
	out := make([]string, len(views))
	for i, v := range views {
		out[i] = fmt.Sprintf(`"%s"."%s"`, v[0], v[1])
	}
	return out
}

// ReadColumnDependents maps each column to the row-level security
// policies that read it: the table's own, a policy on another table that
// reads the column through a subquery, and a policy on a partition, which
// counts for the tree and not for the table alone. A policy whose
// expression names no column does not count against any column. The
// server is the oracle: each policy named for a column refuses its type
// change on its own, and with every named policy dropped the change
// succeeds.
func TestReadColumnDependentsPolicies(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	schema := testutil.NewSchema(t, pool)
	other := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.documents (
			id bigint NOT NULL,
			owner_id varchar(36),
			team_id varchar(36),
			label varchar(100)
		) PARTITION BY RANGE (id);
		CREATE TABLE %[1]s.documents_1 PARTITION OF %[1]s.documents
			FOR VALUES FROM (0) TO (100);
		CREATE TABLE %[2]s.audit (
			id bigint PRIMARY KEY,
			actor varchar(36)
		);
		CREATE POLICY own_rows ON %[1]s.documents
			USING (owner_id = current_user);
		CREATE POLICY "Insert Own" ON %[1]s.documents
			FOR INSERT
			WITH CHECK (owner_id = current_user AND team_id IS NOT NULL);
		CREATE POLICY see_all ON %[1]s.documents
			USING (true);
		CREATE POLICY partition_label ON %[1]s.documents_1
			USING (label <> 'hidden');
		CREATE POLICY actor_documents ON %[2]s.audit
			USING (actor IN (SELECT d.owner_id FROM %[1]s.documents d))`, schema, other))
	require.NoError(t, err)

	deps, err := preflight.ReadColumnDependents(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	onDocuments := func(policy string) string { return fmt.Sprintf(`%s ON "%s"."documents"`, policy, schema) }
	ownerPolicies := []string{onDocuments(`"Insert Own"`), onDocuments(`"own_rows"`), fmt.Sprintf(`"actor_documents" ON "%s"."audit"`, other)}
	if other < schema {
		ownerPolicies = []string{ownerPolicies[2], ownerPolicies[0], ownerPolicies[1]}
	}
	assert.Equal(t, map[string][]string{
		"owner_id": ownerPolicies,
		"team_id":  {onDocuments(`"Insert Own"`)},
	}, deps.Policies.Own)
	assert.Equal(t, map[string][]string{
		"owner_id": ownerPolicies,
		"team_id":  {onDocuments(`"Insert Own"`)},
		"label":    {fmt.Sprintf(`"partition_label" ON "%s"."documents_1"`, schema)},
	}, deps.Policies.Tree)
	assert.Equal(t, deps.Policies.Tree, deps.Policies.Drop, "a partition never declares a column")
	assert.Empty(t, deps.Views.Tree, "no view reads documents")

	retype := func(t *testing.T, column, keep string) error {
		t.Helper()
		tx, err := pool.Begin(t.Context())
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
		for _, policy := range deps.Policies.Tree[column] {
			if policy != keep {
				_, err := tx.Exec(t.Context(), "DROP POLICY "+policy)
				require.NoError(t, err)
			}
		}
		_, err = tx.Exec(t.Context(), fmt.Sprintf("ALTER TABLE %s.documents ALTER COLUMN %s TYPE text", schema, column))
		return err
	}
	for _, column := range []string{"owner_id", "team_id", "label"} {
		require.NoError(t, retype(t, column, ""), "no policy outside Tree blocks retyping %s", column)
		for _, policy := range deps.Policies.Tree[column] {
			var pgErr *pgconn.PgError
			require.ErrorAs(t, retype(t, column, policy), &pgErr, policy)
			assert.Equal(t, "0A000", pgErr.Code, policy)
		}
	}
}

// A recursive DROP COLUMN is blocked by the policies on the copies of the
// column it drops, judged by the same reach as a view: a child that
// declares the column, or inherits it from a parent the drop does not
// touch, keeps its copy, so its policy does not count, while a type change
// reaches every copy. A policy on a table outside the tree never counts.
// The server is the oracle: with the policies Drop names removed the drop
// succeeds, and each policy Drop names blocks it on its own.
func TestReadColumnDependentsPoliciesForADrop(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	schema := testutil.NewSchema(t, pool)
	tables := []string{"users", "inherited", "below_inherited", "declared", "below_declared", "other_parent",
		"two_parents", "attached", "branch_a", "branch_b", "diamond", "uneven", "events_1"}
	var setup strings.Builder
	fmt.Fprintf(&setup, `
		CREATE TABLE %[1]s.users (id bigint PRIMARY KEY, email text);
		CREATE TABLE %[1]s.inherited () INHERITS (%[1]s.users);
		CREATE TABLE %[1]s.below_inherited () INHERITS (%[1]s.inherited);
		CREATE TABLE %[1]s.declared (email text) INHERITS (%[1]s.users);
		CREATE TABLE %[1]s.below_declared () INHERITS (%[1]s.declared);
		CREATE TABLE %[1]s.other_parent (email text);
		CREATE TABLE %[1]s.two_parents () INHERITS (%[1]s.users, %[1]s.other_parent);
		CREATE TABLE %[1]s.attached (id bigint NOT NULL, email text);
		ALTER TABLE %[1]s.attached INHERIT %[1]s.users;
		CREATE TABLE %[1]s.branch_a () INHERITS (%[1]s.users);
		CREATE TABLE %[1]s.branch_b () INHERITS (%[1]s.users);
		CREATE TABLE %[1]s.diamond () INHERITS (%[1]s.branch_a, %[1]s.branch_b);
		CREATE TABLE %[1]s.uneven () INHERITS (%[1]s.below_inherited, %[1]s.branch_b);
		CREATE TABLE %[1]s.events (id bigint, email text) PARTITION BY RANGE (id);
		CREATE TABLE %[1]s.events_1 PARTITION OF %[1]s.events FOR VALUES FROM (0) TO (10);`, schema)
	for _, table := range tables {
		fmt.Fprintf(&setup, "\nCREATE POLICY p_%[2]s ON %[1]s.%[2]s USING (email IS NOT NULL);", schema, table)
	}
	_, err = pool.Exec(t.Context(), setup.String())
	require.NoError(t, err)
	policies := func(tables ...string) []string {
		out := make([]string, len(tables))
		for i, table := range tables {
			out[i] = fmt.Sprintf(`"p_%[2]s" ON "%[1]s"."%[2]s"`, schema, table)
		}
		return out
	}

	deps, err := preflight.ReadColumnDependents(t.Context(), pool, schema, "users")
	require.NoError(t, err)
	assert.Equal(t, policies("attached", "below_declared", "below_inherited", "branch_a", "branch_b", "declared",
		"diamond", "inherited", "two_parents", "uneven", "users"), deps.Policies.Tree["email"])
	assert.Equal(t, policies("below_inherited", "branch_a", "branch_b", "diamond", "inherited", "uneven", "users"),
		deps.Policies.Drop["email"])
	assert.Equal(t, policies("users"), deps.Policies.Own["email"])

	dropFails := func(t *testing.T, keep string) error {
		t.Helper()
		tx, err := pool.Begin(t.Context())
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
		for _, policy := range deps.Policies.Drop["email"] {
			if policy != keep {
				_, err := tx.Exec(t.Context(), "DROP POLICY "+policy)
				require.NoError(t, err)
			}
		}
		_, err = tx.Exec(t.Context(), fmt.Sprintf("ALTER TABLE %s.users DROP COLUMN email", schema))
		return err
	}
	require.NoError(t, dropFails(t, ""), "no policy outside Drop blocks the drop")
	for _, policy := range deps.Policies.Drop["email"] {
		var pgErr *pgconn.PgError
		require.ErrorAs(t, dropFails(t, policy), &pgErr, policy)
		assert.Equal(t, "2BP01", pgErr.Code, policy)
	}

	partitioned, err := preflight.ReadColumnDependents(t.Context(), pool, schema, "events")
	require.NoError(t, err)
	assert.Equal(t, policies("events_1"), partitioned.Policies.Drop["email"], "a partition never declares a column")
}

// ReadColumnDependents maps each column to the triggers whose UPDATE OF
// list or WHEN condition names it, on the table or, for the tree, on an
// inheritance child, and to the generated columns whose expression reads
// it. A trigger whose function body reads a column, a statistics object
// over it, and a generated column's own entry do not count. The server is
// the oracle: each named object blocks the change on its own, with every
// named object removed the change succeeds, and the column nothing names
// retypes with every trigger in place.
func TestReadColumnDependentsTriggersAndGeneratedColumns(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE FUNCTION %[1]s.noop() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;
		CREATE FUNCTION %[1]s.reads_note() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN IF NEW.note IS NULL THEN RAISE EXCEPTION 'note'; END IF; RETURN NEW; END $$;
		CREATE TABLE %[1]s.orders (id bigint PRIMARY KEY, status varchar(16), qty int, note text);
		CREATE TABLE %[1]s.orders_archive () INHERITS (%[1]s.orders);
		CREATE TRIGGER "Audit Status" BEFORE UPDATE OF status ON %[1]s.orders
			FOR EACH ROW EXECUTE FUNCTION %[1]s.noop();
		CREATE TRIGGER qty_positive BEFORE UPDATE ON %[1]s.orders
			FOR EACH ROW WHEN (NEW.qty > 0) EXECUTE FUNCTION %[1]s.noop();
		CREATE TRIGGER body_reads_note BEFORE UPDATE ON %[1]s.orders
			FOR EACH ROW EXECUTE FUNCTION %[1]s.reads_note();
		CREATE TRIGGER archive_status BEFORE UPDATE OF status ON %[1]s.orders_archive
			FOR EACH ROW EXECUTE FUNCTION %[1]s.noop();
		CREATE STATISTICS %[1]s.qty_note ON qty, note FROM %[1]s.orders;
		CREATE TABLE %[1]s.ledger (
			id bigint PRIMARY KEY,
			amount numeric(10, 2),
			tax numeric(10, 2),
			total numeric GENERATED ALWAYS AS (amount + tax) STORED,
			doubled numeric GENERATED ALWAYS AS (amount * 2) STORED
		)`, schema))
	require.NoError(t, err)

	on := func(trigger, table string) string { return fmt.Sprintf(`%s ON "%s"."%s"`, trigger, schema, table) }
	ledger := func(column string) string { return fmt.Sprintf(`"%s"."ledger"."%s"`, schema, column) }

	orders, err := preflight.ReadColumnDependents(t.Context(), pool, schema, "orders")
	require.NoError(t, err)
	assert.Equal(t, map[string][]string{
		"status": {on(`"Audit Status"`, "orders")},
		"qty":    {on(`"qty_positive"`, "orders")},
	}, orders.Triggers.Own)
	tree := map[string][]string{
		"status": {on(`"Audit Status"`, "orders"), on(`"archive_status"`, "orders_archive")},
		"qty":    {on(`"qty_positive"`, "orders")},
	}
	assert.Equal(t, tree, orders.Triggers.Tree)
	assert.Equal(t, tree, orders.Triggers.Drop, "the child inherits status without declaring it")
	assert.Empty(t, orders.GeneratedColumns.Tree)
	assert.Empty(t, orders.PartitionKeys.Tree)

	ledgerDeps, err := preflight.ReadColumnDependents(t.Context(), pool, schema, "ledger")
	require.NoError(t, err)
	generated := map[string][]string{
		"amount": {ledger("doubled"), ledger("total")},
		"tax":    {ledger("total")},
	}
	assert.Equal(t, generated, ledgerDeps.GeneratedColumns.Own)
	assert.Equal(t, generated, ledgerDeps.GeneratedColumns.Tree)
	assert.Empty(t, ledgerDeps.Triggers.Tree)

	// retype runs the change in a rolled-back transaction after removing
	// every named dependent except keep.
	retype := func(t *testing.T, table, column, newType string, named []string, keep string) error {
		t.Helper()
		tx, err := pool.Begin(t.Context())
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
		for _, object := range named {
			if object == keep {
				continue
			}
			drop := "DROP TRIGGER " + object
			if table == "ledger" {
				drop = fmt.Sprintf("ALTER TABLE %s.ledger DROP COLUMN %s", schema, object[strings.LastIndex(object, "."):][1:])
			}
			_, err := tx.Exec(t.Context(), drop)
			require.NoError(t, err, drop)
		}
		_, err = tx.Exec(t.Context(), fmt.Sprintf("ALTER TABLE %s.%s ALTER COLUMN %s TYPE %s", schema, table, column, newType))
		return err
	}
	// PostgreSQL 14 reports a generated column blocking a type change as a
	// syntax error class; later versions report it as unsupported.
	var version int
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT pg_catalog.current_setting('server_version_num')::pg_catalog.int4").Scan(&version))
	generatedCode := "0A000"
	if version < 150000 {
		generatedCode = "42601"
	}
	for _, tc := range []struct {
		table, column, newType string
		named                  []string
	}{
		{"orders", "status", "varchar(64)", orders.Triggers.Tree["status"]},
		{"orders", "qty", "bigint", orders.Triggers.Tree["qty"]},
		{"ledger", "amount", "numeric(12, 2)", ledgerDeps.GeneratedColumns.Tree["amount"]},
		{"ledger", "tax", "numeric(12, 2)", ledgerDeps.GeneratedColumns.Tree["tax"]},
	} {
		require.NoError(t, retype(t, tc.table, tc.column, tc.newType, tc.named, ""),
			"nothing outside the named dependents blocks retyping %s.%s", tc.table, tc.column)
		for _, object := range tc.named {
			var pgErr *pgconn.PgError
			require.ErrorAs(t, retype(t, tc.table, tc.column, tc.newType, tc.named, object), &pgErr, object)
			code := "0A000"
			if tc.table == "ledger" {
				code = generatedCode
			}
			assert.Equal(t, code, pgErr.Code, object)
		}
	}
	require.NoError(t, retype(t, "orders", "note", "varchar(200)", nil, ""),
		"a trigger body and a statistics object do not block the change")
}

// ReadColumnDependents maps each generated column to its own copies: every
// table's for the tree, the table's alone for ONLY, and for a drop the
// copies a recursive DROP COLUMN removes, which leaves out a child that
// declares the column itself. The server is the oracle: a statement that
// drops the generated column and retypes the column it reads runs once
// no copy outside the drop is left, and is refused while one is.
func TestReadColumnDependentsGeneratedColumnCopies(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.ledger (id bigint, amount numeric(10, 2), total numeric GENERATED ALWAYS AS (amount * 2) STORED);
		CREATE TABLE %[1]s.ledger_archive () INHERITS (%[1]s.ledger);
		CREATE TABLE %[1]s.ledger_local (id bigint, amount numeric(10, 2), total numeric GENERATED ALWAYS AS (amount * 2) STORED);
		ALTER TABLE %[1]s.ledger_local INHERIT %[1]s.ledger`, schema))
	require.NoError(t, err)

	copyOf := func(table string) string { return fmt.Sprintf(`"%s"."%s"."total"`, schema, table) }
	deps, err := preflight.ReadColumnDependents(t.Context(), pool, schema, "ledger")
	require.NoError(t, err)
	everyCopy := []string{copyOf("ledger"), copyOf("ledger_archive"), copyOf("ledger_local")}
	assert.Equal(t, map[string][]string{"total": everyCopy}, deps.GeneratedColumnCopies.Tree)
	assert.Equal(t, map[string][]string{"total": {copyOf("ledger"), copyOf("ledger_archive")}}, deps.GeneratedColumnCopies.Drop,
		"ledger_local declares total itself and keeps it")
	assert.Equal(t, map[string][]string{"total": {copyOf("ledger")}}, deps.GeneratedColumnCopies.Own)
	assert.Equal(t, everyCopy, deps.GeneratedColumns.Tree["amount"])

	run := func(t *testing.T, detachLocal bool) error {
		t.Helper()
		tx, err := pool.Begin(t.Context())
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
		if detachLocal {
			_, err := tx.Exec(t.Context(), fmt.Sprintf("ALTER TABLE %[1]s.ledger_local NO INHERIT %[1]s.ledger", schema))
			require.NoError(t, err)
		}
		_, err = tx.Exec(t.Context(), fmt.Sprintf(
			"ALTER TABLE %s.ledger ALTER COLUMN amount TYPE numeric(12, 2), DROP COLUMN total", schema))
		return err
	}
	var pgErr *pgconn.PgError
	require.ErrorAs(t, run(t, false), &pgErr, "the copy the drop leaves still reads amount")
	require.NoError(t, run(t, true), "the drop removes every remaining copy before the type change")
}

// ReadColumnDependents maps each partition key column, named directly or
// used in a key expression, to the partitioned tables whose key includes
// it: the table's own key for the table alone, and a sub-partition's key
// too for the tree. A partition never declares a column, so a recursive
// drop reaches the same tables. The server is the oracle: it refuses to
// retype every column the map names with SQLSTATE 42P16, and retypes a
// column outside every key.
func TestReadColumnDependentsPartitionKeys(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.events (id bigint, region int, created_at timestamp, kind text)
			PARTITION BY RANGE (created_at);
		CREATE TABLE %[1]s.events_2026 PARTITION OF %[1]s.events
			FOR VALUES FROM ('2026-01-01') TO ('2027-01-01') PARTITION BY LIST (region);
		CREATE TABLE %[1]s.events_2026_eu PARTITION OF %[1]s.events_2026 FOR VALUES IN (1);
		CREATE TABLE %[1]s.tickets (id bigint, code text, note text) PARTITION BY LIST (lower(code));
		CREATE TABLE %[1]s.tickets_a PARTITION OF %[1]s.tickets FOR VALUES IN ('a')`, schema))
	require.NoError(t, err)
	table := func(name string) string { return fmt.Sprintf(`"%s"."%s"`, schema, name) }

	events, err := preflight.ReadColumnDependents(t.Context(), pool, schema, "events")
	require.NoError(t, err)
	assert.Equal(t, map[string][]string{"created_at": {table("events")}}, events.PartitionKeys.Own)
	tree := map[string][]string{
		"created_at": {table("events")},
		"region":     {table("events_2026")},
	}
	assert.Equal(t, tree, events.PartitionKeys.Tree)
	assert.Equal(t, tree, events.PartitionKeys.Drop, "a partition never declares a column")

	tickets, err := preflight.ReadColumnDependents(t.Context(), pool, schema, "tickets")
	require.NoError(t, err)
	assert.Equal(t, map[string][]string{"code": {table("tickets")}}, tickets.PartitionKeys.Tree)
	assert.Equal(t, map[string][]string{"code": {table("tickets")}}, tickets.PartitionKeys.Own)

	retype := func(t *testing.T, table, column, newType string) error {
		t.Helper()
		tx, err := pool.Begin(t.Context())
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
		_, err = tx.Exec(t.Context(), fmt.Sprintf("ALTER TABLE %s.%s ALTER COLUMN %s TYPE %s", schema, table, column, newType))
		return err
	}
	for _, tc := range [][3]string{
		{"events", "created_at", "timestamptz"},
		{"events", "region", "bigint"},
		{"tickets", "code", "varchar(20)"},
	} {
		var pgErr *pgconn.PgError
		require.ErrorAs(t, retype(t, tc[0], tc[1], tc[2]), &pgErr, tc[1])
		assert.Equal(t, "42P16", pgErr.Code, tc[1])
	}
	require.NoError(t, retype(t, "events", "kind", "varchar(20)"), "a column outside every key retypes")
	require.NoError(t, retype(t, "tickets", "note", "varchar(20)"), "a column outside every key retypes")
}
