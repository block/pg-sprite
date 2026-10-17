package executor_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/preflight"
)

// The refresh analyzes the whole table, so every column has statistics
// afterwards, and returns the ANALYZE it ran. It waits on a conflicting
// lock no longer than its lock budget: behind a session that holds SHARE
// UPDATE EXCLUSIVE, as a running VACUUM does, it returns a *BudgetError
// instead of queueing, along with the ANALYZE still owed.
func TestRefreshTableStatistics(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	for _, ddl := range []string{
		"CREATE TABLE %[1]s.t (id int PRIMARY KEY, code text, note text)",
		"INSERT INTO %[1]s.t SELECT g, 'c' || (g %% 10), 'n' FROM generate_series(1, 500) g",
	} {
		_, err := pool.Exec(t.Context(), fmt.Sprintf(ddl, schema))
		require.NoError(t, err, ddl)
	}
	pt, err := preflight.CheckTable(t.Context(), pool, schema, "t", preflight.NoSizeLimit)
	require.NoError(t, err)
	budget := executor.Budget{LockTimeout: 100 * time.Millisecond, StatementTimeout: 5 * time.Second}
	want := fmt.Sprintf(`ANALYZE "%s"."t"`, schema)

	analyze, err := executor.RefreshTableStatistics(t.Context(), pool, pt, budget)
	require.NoError(t, err)
	assert.Equal(t, want, analyze)
	var analyzed []string
	rows, err := pool.Query(t.Context(),
		"SELECT attname FROM pg_stats WHERE schemaname = $1 AND tablename = 't' ORDER BY attname", schema)
	require.NoError(t, err)
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		analyzed = append(analyzed, name)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{"code", "id", "note"}, analyzed)

	holder, err := pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() {
		// Redundant closer: the holder is rolled back below on the
		// success path.
		_ = holder.Rollback(t.Context())
	}()
	_, err = holder.Exec(t.Context(), fmt.Sprintf("LOCK TABLE %s.t IN SHARE UPDATE EXCLUSIVE MODE", schema))
	require.NoError(t, err)
	analyze, err = executor.RefreshTableStatistics(t.Context(), pool, pt, budget)
	require.NoError(t, holder.Rollback(t.Context()))
	var budgetErr *executor.BudgetError
	require.True(t, errors.As(err, &budgetErr), "want a *BudgetError, got %v", err)
	assert.Equal(t, want, analyze)
}

// An inheritance child is analyzed with its parent, and the refresh
// refuses before analyzing anything when a child is one it cannot vouch
// for: a foreign table, which ANALYZE skips with only a warning when its
// wrapper cannot sample it. The ANALYZE it names covers every child.
func TestRefreshTableStatisticsCoversInheritanceChildren(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	for _, ddl := range []string{
		"CREATE TABLE %[1]s.p (id int, code text)",
		`CREATE TABLE %[1]s."Child" () INHERITS (%[1]s.p)`,
		`INSERT INTO %[1]s."Child" SELECT g, 'c' || g FROM generate_series(1, 100) g`,
	} {
		_, err := pool.Exec(t.Context(), fmt.Sprintf(ddl, schema))
		require.NoError(t, err, ddl)
	}
	pt, err := preflight.CheckTable(t.Context(), pool, schema, "p", preflight.NoSizeLimit)
	require.NoError(t, err)
	budget := executor.Budget{LockTimeout: time.Second, StatementTimeout: 5 * time.Second}

	analyze, err := executor.RefreshTableStatistics(t.Context(), pool, pt, budget)
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf(`ANALYZE "%[1]s"."p", "%[1]s"."Child"`, schema), analyze)
	var childAnalyzed bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM pg_stats WHERE schemaname = $1 AND tablename = 'Child' AND NOT inherited)`,
		schema).Scan(&childAnalyzed))
	assert.True(t, childAnalyzed)

	wrapper := "fdw_" + schema
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE FOREIGN DATA WRAPPER %[2]s;
		CREATE SERVER %[2]s FOREIGN DATA WRAPPER %[2]s;
		CREATE FOREIGN TABLE %[1]s.remote () INHERITS (%[1]s.p) SERVER %[2]s`, schema, wrapper))
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		if _, err := pool.Exec(ctx, fmt.Sprintf("DROP FOREIGN DATA WRAPPER %s CASCADE", wrapper)); err != nil {
			t.Logf("drop wrapper %s: %v", wrapper, err)
		}
	})
	analyze, err = executor.RefreshTableStatistics(t.Context(), pool, pt, budget)
	require.ErrorIs(t, err, executor.ErrAnalyzeForeignChild)
	assert.Contains(t, err.Error(), fmt.Sprintf(`"%s"."remote"`, schema))
	assert.Equal(t, fmt.Sprintf(`ANALYZE "%[1]s"."p", "%[1]s"."Child", "%[1]s"."remote"`, schema), analyze)
}

// A role that cannot act as the table's owner gets only a warning from
// ANALYZE, which skips the table, so the refresh checks ownership first and
// refuses with ErrAnalyzeNotPermitted instead of reporting statistics it
// never refreshed. The table's statistics stay as they were. Every table
// the refresh covers is checked, so a role that owns an inheritance parent
// but not its child is refused too.
func TestRefreshTableStatisticsRefusesARoleThatCannotActAsTheOwner(t *testing.T) {
	url := testutil.StartPostgres(t)
	superuser, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(superuser.Close)
	schema := testutil.NewSchema(t, superuser)
	role := "reader_" + schema
	_, err = superuser.Exec(t.Context(), fmt.Sprintf(`
		CREATE ROLE %[1]s LOGIN PASSWORD 'reader';
		GRANT USAGE ON SCHEMA %[2]s TO %[1]s;
		CREATE TABLE %[2]s.t (id int PRIMARY KEY, code text);
		INSERT INTO %[2]s.t SELECT g, 'c' || g FROM generate_series(1, 100) g;
		GRANT SELECT, INSERT, UPDATE, DELETE ON %[2]s.t TO %[1]s`, role, schema))
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		if _, err := superuser.Exec(ctx, fmt.Sprintf("DROP OWNED BY %[1]s; DROP ROLE %[1]s", role)); err != nil {
			t.Logf("drop role %s: %v", role, err)
		}
	})
	reader, err := dbconn.NewPool(t.Context(), dbconn.Config{
		URL: url,
		BeforeConnect: func(_ context.Context, cc *pgx.ConnConfig) error {
			cc.User = role
			cc.Password = "reader"
			return nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(reader.Close)
	pt, err := preflight.CheckTable(t.Context(), reader, schema, "t", preflight.NoSizeLimit)
	require.NoError(t, err)

	_, err = executor.RefreshTableStatistics(t.Context(), reader, pt,
		executor.Budget{LockTimeout: time.Second, StatementTimeout: 5 * time.Second})
	require.ErrorIs(t, err, executor.ErrAnalyzeNotPermitted)
	assert.Contains(t, err.Error(), fmt.Sprintf(`"%s"."t"`, schema))
	var analyzed int
	require.NoError(t, superuser.QueryRow(t.Context(),
		"SELECT count(*) FROM pg_stats WHERE schemaname = $1 AND tablename = 't'", schema).Scan(&analyzed))
	assert.Zero(t, analyzed, "nothing was analyzed")

	_, err = superuser.Exec(t.Context(), fmt.Sprintf(`
		ALTER TABLE %[2]s.t OWNER TO %[1]s;
		CREATE TABLE %[2]s.child () INHERITS (%[2]s.t)`, role, schema))
	require.NoError(t, err)
	_, err = executor.RefreshTableStatistics(t.Context(), reader, pt,
		executor.Budget{LockTimeout: time.Second, StatementTimeout: 5 * time.Second})
	require.ErrorIs(t, err, executor.ErrAnalyzeNotPermitted)
	assert.Contains(t, err.Error(), fmt.Sprintf(`"%s"."child"`, schema))
	require.NoError(t, superuser.QueryRow(t.Context(),
		"SELECT count(*) FROM pg_stats WHERE schemaname = $1", schema).Scan(&analyzed))
	assert.Zero(t, analyzed, "nothing was analyzed")
}
