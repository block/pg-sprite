package schemadiff_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// foreignKeyPool returns a pool and a schema of the test's own holding the
// given tables, each statement formatted with the schema's name.
func foreignKeyPool(t *testing.T, ddl ...string) (*pgxpool.Pool, string) {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	for _, stmt := range ddl {
		_, err := pool.Exec(t.Context(), fmt.Sprintf(stmt, schema))
		require.NoError(t, err)
	}
	return pool, schema
}

// roundTripWithForeignKeys renders the live table with its foreign keys,
// materializes the file on the scratch schema, and returns the file and the
// diff from the live table to it.
func roundTripWithForeignKeys(t *testing.T, pool *pgxpool.Pool, schema, table string) (string, []schemadiff.Change) {
	t.Helper()
	live, err := schemadiff.Introspect(t.Context(), pool, schema, table)
	require.NoError(t, err)
	rendered, err := schemadiff.RenderWith(live, schemadiff.RenderOptions{ForeignKeys: true})
	require.NoError(t, err)
	ds, err := statement.ParseDesired(rendered)
	require.NoError(t, err, "rendered output must parse as a desired schema:\n%s", rendered)
	desired, err := schemadiff.IntrospectDesired(t.Context(), pool, schema, ds)
	require.NoError(t, err, "rendered output must materialize on the scratch schema:\n%s", rendered)
	changes, err := schemadiff.Diff(schema, live, desired)
	require.NoError(t, err)
	return rendered, changes
}

// With foreign keys opted in, every table of a typical app renders and
// round-trips to an empty diff: the referencing table with its foreign keys
// and their actions, the table they reference, and a table that references
// itself.
func TestRenderWithForeignKeysRoundTrips(t *testing.T) {
	pool, schema := foreignKeyPool(t,
		"CREATE TABLE %[1]s.users (id bigint PRIMARY KEY, email text NOT NULL UNIQUE)",
		`CREATE TABLE %[1]s.orders (
			id bigint PRIMARY KEY,
			user_id bigint NOT NULL REFERENCES %[1]s.users ON DELETE CASCADE,
			buyer_email text REFERENCES %[1]s.users (email) ON UPDATE CASCADE DEFERRABLE INITIALLY DEFERRED
		)`,
		"CREATE TABLE %[1]s.employees (id bigint PRIMARY KEY, manager_id bigint REFERENCES %[1]s.employees (id))",
	)

	orders, changes := roundTripWithForeignKeys(t, pool, schema, "orders")
	assert.Empty(t, changes)
	assert.Contains(t, orders, `CONSTRAINT "orders_user_id_fkey" FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE`)
	assert.Contains(t, orders, `CONSTRAINT "orders_buyer_email_fkey" FOREIGN KEY (buyer_email) REFERENCES users(email) ON UPDATE CASCADE DEFERRABLE INITIALLY DEFERRED`)

	users, changes := roundTripWithForeignKeys(t, pool, schema, "users")
	assert.Empty(t, changes)
	assert.NotContains(t, users, "FOREIGN KEY", "an incoming foreign key belongs to the referencing table's file")

	employees, changes := roundTripWithForeignKeys(t, pool, schema, "employees")
	assert.Empty(t, changes)
	assert.Contains(t, employees, `CONSTRAINT "employees_manager_id_fkey" FOREIGN KEY (manager_id) REFERENCES employees(id)`)
}

// A foreign key the live table has not validated renders as the validated
// one a file declares, so the diff plans its validation rather than hiding
// it, as for a CHECK constraint.
func TestRenderWithForeignKeysPlansValidationOfANotValidKey(t *testing.T) {
	pool, schema := foreignKeyPool(t,
		"CREATE TABLE %[1]s.users (id bigint PRIMARY KEY)",
		"CREATE TABLE %[1]s.orders (id bigint PRIMARY KEY, user_id bigint)",
		"ALTER TABLE %[1]s.orders ADD CONSTRAINT orders_user_fkey FOREIGN KEY (user_id) REFERENCES %[1]s.users NOT VALID",
	)

	rendered, changes := roundTripWithForeignKeys(t, pool, schema, "orders")
	assert.NotContains(t, rendered, "NOT VALID")
	require.Len(t, changes, 1)
	assert.Equal(t, schemadiff.ChangeValidateForeignKey, changes[0].Kind)
}

// A foreign key into another schema is refused, because a desired file names
// tables only in its own schema. That holds for a reference into public too,
// which introspection prints unqualified because public is on its search_path.
func TestRenderWithForeignKeysRefusesACrossSchemaReference(t *testing.T) {
	pool, schema := foreignKeyPool(t)
	other := testutil.NewSchema(t, pool)
	publicUsers := "users_" + schema
	t.Cleanup(func() {
		_, err := pool.Exec(context.WithoutCancel(t.Context()), "DROP TABLE IF EXISTS public."+pgx.Identifier{publicUsers}.Sanitize()+" CASCADE")
		assert.NoError(t, err)
	})
	for name, referenced := range map[string]string{
		"another schema": pgx.Identifier{other, "users"}.Sanitize(),
		"public":         pgx.Identifier{"public", publicUsers}.Sanitize(),
	} {
		t.Run(name, func(t *testing.T) {
			table := "orders_" + strings.ReplaceAll(name, " ", "_")
			for _, ddl := range []string{
				fmt.Sprintf("CREATE TABLE %s (id bigint PRIMARY KEY)", referenced),
				fmt.Sprintf("CREATE TABLE %s (id bigint PRIMARY KEY, user_id bigint REFERENCES %s)", pgx.Identifier{schema, table}.Sanitize(), referenced),
			} {
				_, err := pool.Exec(t.Context(), ddl)
				require.NoError(t, err)
			}

			live, err := schemadiff.Introspect(t.Context(), pool, schema, table)
			require.NoError(t, err)
			_, err = schemadiff.RenderWith(live, schemadiff.RenderOptions{ForeignKeys: true})
			require.ErrorIs(t, err, statement.ErrQualifiedName)
			assert.Contains(t, err.Error(), table+"_user_id_fkey")
		})
	}
}

// A foreign key from a table to itself is refused when the columns it
// references are unique only through a standalone unique index: the file's
// CREATE TABLE would declare the key before the index exists, so the file
// could not create the table. Declared as a UNIQUE constraint instead, the
// same relationship renders and round-trips, even when a standalone unique
// index on the same columns was created first and the server bound the key
// to it. A deferrable UNIQUE constraint on the columns does not count, since
// a foreign key cannot reference one.
func TestRenderWithForeignKeysRefusesASelfReferenceThroughAStandaloneIndex(t *testing.T) {
	pool, schema := foreignKeyPool(t,
		`CREATE TABLE %[1]s.accounts (id bigint PRIMARY KEY, code text NOT NULL, parent_code text)`,
		`CREATE UNIQUE INDEX accounts_code_key ON %[1]s.accounts (code)`,
		`ALTER TABLE %[1]s.accounts ADD CONSTRAINT accounts_parent_fk FOREIGN KEY (parent_code) REFERENCES %[1]s.accounts (code)`,
		`CREATE TABLE %[1]s.ledgers (id bigint PRIMARY KEY, code text NOT NULL CONSTRAINT ledgers_code_key UNIQUE, parent_code text)`,
		`ALTER TABLE %[1]s.ledgers ADD CONSTRAINT ledgers_parent_fk FOREIGN KEY (parent_code) REFERENCES %[1]s.ledgers (code)`,
		`CREATE TABLE %[1]s.journals (id bigint PRIMARY KEY, code text NOT NULL, parent_code text)`,
		`CREATE UNIQUE INDEX journals_code_idx ON %[1]s.journals (code)`,
		`ALTER TABLE %[1]s.journals ADD CONSTRAINT journals_code_key UNIQUE (code)`,
		`ALTER TABLE %[1]s.journals ADD CONSTRAINT journals_parent_fk FOREIGN KEY (parent_code) REFERENCES %[1]s.journals (code)`,
		`CREATE TABLE %[1]s.registers (id bigint PRIMARY KEY, code text NOT NULL, parent_code text)`,
		`CREATE UNIQUE INDEX registers_code_idx ON %[1]s.registers (code)`,
		`ALTER TABLE %[1]s.registers ADD CONSTRAINT registers_code_key UNIQUE (code) DEFERRABLE`,
		`ALTER TABLE %[1]s.registers ADD CONSTRAINT registers_parent_fk FOREIGN KEY (parent_code) REFERENCES %[1]s.registers (code)`)

	for _, table := range []string{"accounts", "registers"} {
		live, err := schemadiff.Introspect(t.Context(), pool, schema, table)
		require.NoError(t, err)
		_, err = schemadiff.RenderWith(live, schemadiff.RenderOptions{ForeignKeys: true})
		require.ErrorIs(t, err, schemadiff.ErrUnrenderableForeignKey, table)
		assert.Contains(t, err.Error(), `"`+table+`_parent_fk"`)
	}

	rendered, changes := roundTripWithForeignKeys(t, pool, schema, "ledgers")
	assert.Contains(t, rendered, `CONSTRAINT "ledgers_parent_fk" FOREIGN KEY (parent_code) REFERENCES ledgers(code)`)
	assert.Empty(t, changes)

	rendered, changes = roundTripWithForeignKeys(t, pool, schema, "journals")
	assert.Contains(t, rendered, `CONSTRAINT "journals_parent_fk" FOREIGN KEY (parent_code) REFERENCES journals(code)`)
	assert.Empty(t, changes)
}

// A row-security file cannot carry a foreign key, so a table with both is
// refused rather than rendered without one of them.
func TestRenderWithForeignKeysAndRowSecurityRefusesAnOutgoingKey(t *testing.T) {
	pool, schema := foreignKeyPool(t,
		"CREATE TABLE %[1]s.users (id bigint PRIMARY KEY)",
		"CREATE TABLE %[1]s.orders (id bigint PRIMARY KEY, user_id bigint REFERENCES %[1]s.users)",
		"ALTER TABLE %[1]s.orders ENABLE ROW LEVEL SECURITY",
	)

	live, err := schemadiff.Introspect(t.Context(), pool, schema, "orders")
	require.NoError(t, err)
	_, err = schemadiff.RenderWith(live, schemadiff.RenderOptions{ForeignKeys: true, RowSecurity: true})
	require.ErrorIs(t, err, statement.ErrRowSecurityForeignKey)
}
