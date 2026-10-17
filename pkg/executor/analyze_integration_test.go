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

// The refresh analyzes only the columns it names, and waits on a conflicting
// lock no longer than its lock budget: behind a session that holds SHARE
// UPDATE EXCLUSIVE, as a running VACUUM does, it returns a *BudgetError
// instead of queueing.
func TestRefreshColumnStatistics(t *testing.T) {
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

	require.NoError(t, executor.RefreshColumnStatistics(t.Context(), pool, pt, []string{"code"}, budget))
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
	assert.Equal(t, []string{"code"}, analyzed)

	holder, err := pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() {
		// Redundant closer: the holder is rolled back below on the
		// success path.
		_ = holder.Rollback(t.Context())
	}()
	_, err = holder.Exec(t.Context(), fmt.Sprintf("LOCK TABLE %s.t IN SHARE UPDATE EXCLUSIVE MODE", schema))
	require.NoError(t, err)
	err = executor.RefreshColumnStatistics(t.Context(), pool, pt, []string{"note"}, budget)
	require.NoError(t, holder.Rollback(t.Context()))
	var budgetErr *executor.BudgetError
	require.True(t, errors.As(err, &budgetErr), "want a *BudgetError, got %v", err)
	assert.Contains(t, err.Error(), fmt.Sprintf(`ANALYZE "%s"."t" ("note")`, schema))
}

// A role that cannot act as the table's owner gets only a warning from
// ANALYZE, which skips the table, so the refresh checks ownership first and
// refuses with ErrAnalyzeNotPermitted instead of reporting statistics it
// never refreshed. The table's statistics stay as they were.
func TestRefreshColumnStatisticsRefusesARoleThatCannotActAsTheOwner(t *testing.T) {
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

	err = executor.RefreshColumnStatistics(t.Context(), reader, pt, []string{"code"},
		executor.Budget{LockTimeout: time.Second, StatementTimeout: 5 * time.Second})
	require.ErrorIs(t, err, executor.ErrAnalyzeNotPermitted)
	assert.Contains(t, err.Error(), fmt.Sprintf(`"%s"."t"`, schema))
	var analyzed int
	require.NoError(t, superuser.QueryRow(t.Context(),
		"SELECT count(*) FROM pg_stats WHERE schemaname = $1 AND tablename = 't'", schema).Scan(&analyzed))
	assert.Zero(t, analyzed, "nothing was analyzed")
}
