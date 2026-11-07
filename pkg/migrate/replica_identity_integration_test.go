package migrate_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/verdict"
)

// columnExists reports whether the named column is present on the table.
func columnExists(t *testing.T, pool *pgxpool.Pool, schema, table, column string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		                WHERE table_schema = $1 AND table_name = $2 AND column_name = $3)`,
		schema, table, column).Scan(&exists))
	return exists
}

// tableExists reports whether the named table is present in the schema.
func tableExists(t *testing.T, pool *pgxpool.Pool, schema, table string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_tables
		                WHERE schemaname = $1 AND tablename = $2)`,
		schema, table).Scan(&exists))
	return exists
}

// A table whose REPLICA IDENTITY USING INDEX selects a unique index keeps
// that setting when the index goes, and every UPDATE and DELETE on it then
// fails while a publication publishes them. Neither front door path drops
// it: the planner refuses dropping a key column or the owning constraint,
// and an accepted blocking DROP INDEX is refused under the table's lock,
// including a partitioned parent's index that a partition's identity index
// is attached under. Nothing is executed, and the identity stays usable.
func TestRunRefusesDroppingTheReplicaIdentity(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	seed := func(t *testing.T) string {
		t.Helper()
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`
			CREATE TABLE %[1]s.accounts (
				id bigint PRIMARY KEY,
				tenant bigint NOT NULL,
				ref text NOT NULL,
				note text,
				CONSTRAINT accounts_ref_key UNIQUE (tenant, ref)
			);
			CREATE UNIQUE INDEX accounts_note_key ON %[1]s.accounts (tenant, note) WHERE note IS NOT NULL;
			ALTER TABLE %[1]s.accounts REPLICA IDENTITY USING INDEX accounts_ref_key`, schema))
		require.NoError(t, err)
		return schema
	}
	assertRefused := func(t *testing.T, v verdict.Verdict, schema string) {
		t.Helper()
		assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
		assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)
		assert.Equal(t, verdict.ClassCapabilityBoundary, v.Class)
		assert.False(t, v.BlockingPassthrough)
		assert.Contains(t, v.Detail, "replica identity")
		assert.Contains(t, v.Detail, "REPLICA IDENTITY")
		assert.True(t, indexExists(t, pool, schema, "accounts_ref_key"), "the identity index must still exist")
	}

	t.Run("dropping a key column", func(t *testing.T) {
		schema := seed(t)
		v, err := migrate.Run(t.Context(), pool,
			parseOne(t, fmt.Sprintf("ALTER TABLE %s.accounts DROP COLUMN ref", schema)), runOptions())
		require.NoError(t, err)
		assertRefused(t, v, schema)
		assert.True(t, columnExists(t, pool, schema, "accounts", "ref"))
	})

	t.Run("dropping the owning constraint", func(t *testing.T) {
		schema := seed(t)
		v, err := migrate.Run(t.Context(), pool,
			parseOne(t, fmt.Sprintf("ALTER TABLE %s.accounts DROP CONSTRAINT accounts_ref_key", schema)), runOptions())
		require.NoError(t, err)
		assertRefused(t, v, schema)
	})

	t.Run("an accepted blocking DROP INDEX", func(t *testing.T) {
		schema := seed(t)
		v, err := migrate.Run(t.Context(), pool,
			parseOne(t, fmt.Sprintf("DROP INDEX %s.accounts_ref_key", schema)),
			acceptBlockingOptions(schema+".accounts"))
		require.NoError(t, err)
		assertRefused(t, v, schema)
		assert.Equal(t, schema+".accounts", v.Table)
	})

	t.Run("dropping another column and index still runs", func(t *testing.T) {
		schema := seed(t)
		v, err := migrate.Run(t.Context(), pool,
			parseOne(t, fmt.Sprintf("DROP INDEX %s.accounts_note_key", schema)),
			acceptBlockingOptions(schema+".accounts"))
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecutedWithoutOnlineSafety, v.Outcome)
		assert.False(t, indexExists(t, pool, schema, "accounts_note_key"))

		v, err = migrate.Run(t.Context(), pool,
			parseOne(t, fmt.Sprintf("ALTER TABLE %s.accounts DROP COLUMN note", schema)), destructiveOptions(schema+".accounts"))
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecuted, v.Outcome, "%s", v.Detail)
		assert.False(t, columnExists(t, pool, schema, "accounts", "note"))
	})

	t.Run("a partitioned parent index a partition's identity is attached under", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`
			CREATE TABLE %[1]s.events (id bigint NOT NULL, ref text NOT NULL) PARTITION BY RANGE (id);
			CREATE TABLE %[1]s.events_1 PARTITION OF %[1]s.events FOR VALUES FROM (0) TO (100);
			CREATE UNIQUE INDEX events_ref_key ON %[1]s.events (id, ref);
			ALTER TABLE %[1]s.events_1 REPLICA IDENTITY USING INDEX events_1_id_ref_idx`, schema))
		require.NoError(t, err)

		v, err := migrate.Run(t.Context(), pool,
			parseOne(t, fmt.Sprintf("DROP INDEX %s.events_ref_key", schema)),
			acceptBlockingOptions(schema+".events"))
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
		assert.Contains(t, v.Detail, "events_1_id_ref_idx")
		assert.True(t, indexExists(t, pool, schema, "events_ref_key"))
		assert.True(t, indexExists(t, pool, schema, "events_1_id_ref_idx"))
	})
}

// Under REPLICA IDENTITY DEFAULT the primary key is the table's identity,
// and PostgreSQL drops it without complaint, so a published table is left
// with no identity and its next UPDATE fails with SQLSTATE 55000. The
// planner refuses dropping the primary key, or a column it covers, while a
// publication publishes the table's updates or deletes. The same drop on a
// table no publication publishes runs.
func TestRunRefusesDroppingThePrimaryKeyOfAPublishedDefaultIdentity(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	seed := func(t *testing.T, published bool) string {
		t.Helper()
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`
			CREATE TABLE %[1]s.orders (id bigint PRIMARY KEY, note text);
			INSERT INTO %[1]s.orders VALUES (1, 'first')`, schema))
		require.NoError(t, err)
		if published {
			publication := pgx.Identifier{schema + "_pub"}.Sanitize()
			t.Cleanup(func() {
				// t.Context is cancelled by cleanup time; strip the cancellation.
				if _, err := pool.Exec(context.WithoutCancel(t.Context()), "DROP PUBLICATION IF EXISTS "+publication); err != nil {
					t.Logf("drop publication %s: %v", publication, err)
				}
			})
			_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE PUBLICATION %s FOR TABLE %s.orders", publication, schema))
			require.NoError(t, err)
		}
		return schema
	}

	t.Run("the server breaks updates once the primary key is gone", func(t *testing.T) {
		schema := seed(t, true)
		tx, err := pool.Begin(t.Context())
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
		_, err = tx.Exec(t.Context(), fmt.Sprintf("ALTER TABLE %s.orders DROP CONSTRAINT orders_pkey", schema))
		require.NoError(t, err, "PostgreSQL drops the primary key of a published table without complaint")
		_, err = tx.Exec(t.Context(), fmt.Sprintf("UPDATE %s.orders SET note = 'second' WHERE id = 1", schema))
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		assert.Equal(t, "55000", pgErr.Code, pgErr.Message)
	})

	for _, tc := range []struct{ name, ddl string }{
		{"dropping the primary key", "ALTER TABLE %s.orders DROP CONSTRAINT orders_pkey"},
		{"dropping a column the primary key covers", "ALTER TABLE %s.orders DROP COLUMN id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema := seed(t, true)
			v, err := migrate.Run(t.Context(), pool, parseOne(t, fmt.Sprintf(tc.ddl, schema)), destructiveOptions(schema+".orders"))
			require.NoError(t, err)
			assert.Equal(t, verdict.OutcomeRefused, v.Outcome, v.Detail)
			assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)
			assert.Contains(t, v.Detail, "replica identity")
			assert.True(t, indexExists(t, pool, schema, "orders_pkey"), "the primary key must still exist")
			_, err = pool.Exec(t.Context(), fmt.Sprintf("UPDATE %s.orders SET note = 'second' WHERE id = 1", schema))
			assert.NoError(t, err, "the table must still take updates")
		})
	}

	t.Run("dropping the primary key of a table no publication publishes runs", func(t *testing.T) {
		schema := seed(t, false)
		v, err := migrate.Run(t.Context(), pool,
			parseOne(t, fmt.Sprintf("ALTER TABLE %s.orders DROP CONSTRAINT orders_pkey", schema)), destructiveOptions(schema+".orders"))
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecuted, v.Outcome, v.Detail)
		assert.False(t, indexExists(t, pool, schema, "orders_pkey"))
	})
}
