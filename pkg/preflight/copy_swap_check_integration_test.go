package preflight_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
)

// The fold admits an admitted table on a sufficient cluster and mints the
// same target the three checks mint by hand, carrying the run mode it was
// verified for.
func TestCheckCopySwapMintsTheTargetTheThreeChecksMint(t *testing.T) {
	f := newCopySwapEnvironmentFixture(t, testutil.StartPostgresWithSettings(t, "wal_level=logical"))
	env := preflight.CopySwapEnvironment{LogicalDecoding: true, FreeDiskBytes: unlimitedDisk}

	target, err := preflight.CheckCopySwap(t.Context(), f.pool, f.schema, "ledger", env)
	require.NoError(t, err)
	byHand, err := preflight.CheckCopySwapEnvironment(t.Context(), f.pool, f.shape, env)
	require.NoError(t, err)
	assert.Equal(t, byHand, target)
	assert.True(t, target.DecodesWAL())
	assert.Equal(t, "ledger", target.Table())
	assert.Equal(t, preflight.PKBigint, target.PKType())
}

// Each check refuses in its own vocabulary, so a caller of the fold still
// sees which of privileges, shape, or environment turned the run away: an
// engine role with no access is a privilege refusal, a composite key is a
// shape refusal, and an unmeasured volume is an environment refusal.
func TestCheckCopySwapReportsEachCheckInItsOwnVocabulary(t *testing.T) {
	sufficient := preflight.CopySwapEnvironment{FreeDiskBytes: unlimitedDisk}

	t.Run("privileges", func(t *testing.T) {
		f := newPrivilegeFixture(t)
		_, err := preflight.CheckCopySwap(t.Context(), f.engine, f.schema, "target", sufficient)
		var privErr *preflight.PrivilegeError
		require.ErrorAs(t, err, &privErr)
	})

	t.Run("shape", func(t *testing.T) {
		pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
		require.NoError(t, err)
		t.Cleanup(pool.Close)
		schema := testutil.NewSchema(t, pool)
		_, err = pool.Exec(t.Context(), fmt.Sprintf(`
			CREATE TABLE %s.pairs (
				left_id bigint,
				right_id bigint,
				PRIMARY KEY (left_id, right_id)
			)`, schema))
		require.NoError(t, err)

		_, err = preflight.CheckCopySwap(t.Context(), pool, schema, "pairs", sufficient)
		requireCopySwapCause(t, err, preflight.CopySwapCausePKUnsupported)
	})

	t.Run("environment", func(t *testing.T) {
		f := newCopySwapEnvironmentFixture(t, testutil.StartPostgres(t))
		_, err := preflight.CheckCopySwap(t.Context(), f.pool, f.schema, "ledger", preflight.CopySwapEnvironment{FreeDiskBytes: 0})
		requireCopySwapEnvironmentCause(t, err, preflight.CopySwapCauseDiskHeadroom, "")
	})
}
