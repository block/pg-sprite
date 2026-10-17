package executor_test

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
	"github.com/block/pg-sprite/pkg/executor"
)

// newShadowedPathPool returns a pool whose sessions list schema ahead of
// pg_catalog, the path an operator's role or database setting can produce. It
// is a raw pgx pool on purpose: pkg/dbconn pools rewrite that path before any
// query runs, and the read under test must hold on a pool the library caller
// built without that hook.
func newShadowedPathPool(t *testing.T, serverURL, schema string) *pgxpool.Pool {
	t.Helper()
	pc, err := pgxpool.ParseConfig(serverURL)
	require.NoError(t, err, "parse server URL")
	pc.ConnConfig.RuntimeParams["search_path"] = pgx.Identifier{schema}.Sanitize() + ", pg_catalog"
	pool, err := pgxpool.NewWithConfig(t.Context(), pc)
	require.NoError(t, err, "connect with a shadowing search_path")
	t.Cleanup(pool.Close)
	return pool
}

// A pg_roles view planted in the schema that the session path lists ahead of
// pg_catalog names the create owner as the owner of every role. Unqualified,
// the owner verification would read the table's owner through it and pass a
// table the CREATE's own transaction handed to another role; qualified, it
// reads the real catalog and reports the mismatch.
func TestExecuteCreateWithOwnerVerifiesOwnerThroughTheRealCatalog(t *testing.T) {
	f, owner := newCreateOwnerFixture(t, "t")
	other := testutil.NewRole(t, f.pool, "NOLOGIN")
	_, err := f.pool.Exec(t.Context(), fmt.Sprintf("GRANT %s TO %s; GRANT USAGE, CREATE ON SCHEMA %s TO %s",
		pgx.Identifier{other}.Sanitize(), pgx.Identifier{owner}.Sanitize(),
		pgx.Identifier{f.schema}.Sanitize(), pgx.Identifier{other}.Sanitize()))
	require.NoError(t, err)
	t.Cleanup(func() {
		// The second role is created after the fixture, so its drop runs
		// before the fixture's schema drop; release the schema first.
		_, err := f.pool.Exec(context.WithoutCancel(t.Context()),
			fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", pgx.Identifier{f.schema}.Sanitize()))
		assert.NoError(t, err)
	})
	_, err = f.pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE VIEW %s.pg_roles AS
		SELECT oid, %s::name AS rolname
		FROM pg_catalog.pg_roles`,
		pgx.Identifier{f.schema}.Sanitize(), quoteLiteral(owner)))
	require.NoError(t, err)
	shadowed := newShadowedPathPool(t, f.pool.Config().ConnString(), f.schema)

	var answered string
	require.NoError(t, shadowed.QueryRow(t.Context(),
		`SELECT rolname FROM pg_roles ORDER BY oid LIMIT 1`).Scan(&answered))
	require.Equal(t, owner, answered, "the planted view must answer an unqualified pg_roles on this pool")

	testutil.RunDuringDDL(t, f.pool, testutil.DDLCommandEnd, "CREATE TABLE", f.schema, "t",
		fmt.Sprintf("ALTER TABLE %s.t OWNER TO %s", pgx.Identifier{f.schema}.Sanitize(), pgx.Identifier{other}.Sanitize()))

	rep, err := executor.ExecuteCreate(t.Context(), shadowed, f.at, f.cr,
		desired(t, "CREATE TABLE t (a int, v text)"), createBudget, executor.DefaultRetryPolicy())
	var mismatch *executor.CreateOwnerMismatchError
	require.ErrorAs(t, err, &mismatch)
	assert.Equal(t, owner, mismatch.Expected)
	assert.Equal(t, other, mismatch.Actual)
	assert.Empty(t, rep.Steps)
	assert.Equal(t, other, relationOwner(t, f.pool, f.schema, "t"), "the committed owner is reported, not repaired")
}

// quoteLiteral renders s as a SQL string literal for a fixture role name.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
