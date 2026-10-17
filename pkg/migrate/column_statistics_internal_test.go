package migrate

import (
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/statement"
)

// The type change is committed before its statistics are refreshed, so a
// refresh that cannot run does not fail the run: the verdict's detail names
// the ANALYZE the operator still has to run. Another session's VACUUM or
// ANALYZE holds the same lock the refresh needs; here a transaction holding
// SHARE UPDATE EXCLUSIVE stands in for it.
func TestRefreshRetypedStatisticsNamesTheAnalyzeItCouldNotRun(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, code varchar(100))", schema))
	require.NoError(t, err)
	pt, err := preflight.CheckTable(t.Context(), pool, schema, "t", preflight.NoSizeLimit)
	require.NoError(t, err)
	st, err := statement.ParseOne(fmt.Sprintf("ALTER TABLE %s.t ALTER COLUMN code TYPE varchar(100)", schema))
	require.NoError(t, err)

	holder, err := pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() {
		// Redundant closer: the holder is rolled back below on the
		// success path.
		_ = holder.Rollback(t.Context())
	}()
	_, err = holder.Exec(t.Context(), fmt.Sprintf("LOCK TABLE %s.t IN SHARE UPDATE EXCLUSIVE MODE", schema))
	require.NoError(t, err)

	budget := executor.Budget{LockTimeout: 100 * time.Millisecond, StatementTimeout: 5 * time.Second}
	detail := refreshRetypedStatistics(t.Context(), pool, pt, st, budget, slog.New(slog.DiscardHandler))
	require.NoError(t, holder.Rollback(t.Context()))

	assert.Contains(t, detail, "the statistics of code were not refreshed after the type change")
	assert.Contains(t, detail, fmt.Sprintf("run ANALYZE %s.t (code)", schema))
}
