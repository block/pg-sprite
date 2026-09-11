package migrate_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/verdict"
)

// Native changes must preserve both the catalog policy and its observable
// tenant isolation. A table-owner query alone cannot prove RLS enforcement.
func TestNativeChangesPreserveRowSecurity(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	role := testutil.NewRole(t, pool, "NOLOGIN")
	var owner string
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT current_user").Scan(&owner))
	_, err = pool.Exec(t.Context(), "GRANT "+pgx.Identifier{role}.Sanitize()+" TO "+pgx.Identifier{owner}.Sanitize())
	require.NoError(t, err)
	schema := testutil.NewSchema(t, pool)
	table := pgx.Identifier{schema, "documents"}.Sanitize()
	for _, sql := range []string{
		"CREATE TABLE " + table + " (id int PRIMARY KEY, tenant text NOT NULL, body text)",
		"INSERT INTO " + table + " VALUES (1, 'a', 'first'), (2, 'b', 'second')",
		"ALTER TABLE " + table + " ENABLE ROW LEVEL SECURITY",
		"CREATE POLICY tenant_rows ON " + table + " USING (tenant = current_setting('app.tenant')) WITH CHECK (tenant = current_setting('app.tenant'))",
		"GRANT USAGE ON SCHEMA " + pgx.Identifier{schema}.Sanitize() + " TO " + pgx.Identifier{role}.Sanitize(),
		"GRANT SELECT ON " + table + " TO " + pgx.Identifier{role}.Sanitize(),
	} {
		_, err := pool.Exec(t.Context(), sql)
		require.NoError(t, err)
	}
	assertIsolation := func() {
		t.Helper()
		var total int
		require.NoError(t, pool.QueryRow(t.Context(), "SELECT count(*) FROM "+table).Scan(&total))
		assert.Equal(t, 2, total)
		var enabled bool
		require.NoError(t, pool.QueryRow(t.Context(), "SELECT relrowsecurity FROM pg_class WHERE oid = $1::regclass", table).Scan(&enabled))
		assert.True(t, enabled)
		var policies int
		require.NoError(t, pool.QueryRow(t.Context(), "SELECT count(*) FROM pg_policy WHERE polrelid = $1::regclass AND polname = 'tenant_rows'", table).Scan(&policies))
		assert.Equal(t, 1, policies)
		tx, err := pool.Begin(t.Context())
		require.NoError(t, err)
		defer func() {
			if err := tx.Rollback(context.WithoutCancel(t.Context())); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
				t.Errorf("rollback security assertion: %v", err)
			}
		}()
		_, err = tx.Exec(t.Context(), "SET LOCAL ROLE "+pgx.Identifier{role}.Sanitize())
		require.NoError(t, err)
		_, err = tx.Exec(t.Context(), "SET LOCAL app.tenant = 'a'")
		require.NoError(t, err)
		var ids []int
		require.NoError(t, tx.QueryRow(t.Context(), "SELECT array_agg(id ORDER BY id) FROM "+table).Scan(&ids))
		assert.Equal(t, []int{1}, ids)
		require.NoError(t, tx.Rollback(t.Context()))
	}
	assertIsolation()
	for _, sql := range []string{
		"ALTER TABLE " + table + " ADD COLUMN title text",
		fmt.Sprintf("CREATE INDEX documents_body ON %s (body)", table),
	} {
		v, err := migrate.Run(t.Context(), pool, parseOne(t, sql), runOptions())
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
		assertIsolation()
	}
}
