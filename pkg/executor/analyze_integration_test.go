package executor_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

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
