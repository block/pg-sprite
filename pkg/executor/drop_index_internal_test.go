package executor

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemadiff"
)

// Another session renames the checked index aside and gives its name to a
// new index after every pre-drop check has passed and before the drop runs.
// The drop removes the new index, by name. The executor reports the
// permanent dropped-index-replaced outcome, and the index it checked is
// still in the catalog under its new name.
func TestDropIndexConcurrentlyReportsAnIndexReplacedBeforeTheDrop(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.t (id int PRIMARY KEY, a int, b int);
		CREATE INDEX t_a_idx ON %[1]s.t (a)`, schema))
	require.NoError(t, err)
	var checked uint32
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT $1::regclass::oid`, schema+".t_a_idx").Scan(&checked))
	reviewed, found, err := schemadiff.ReadDroppedObject(t.Context(), pool, schema, "t", schemadiff.DroppedIndex, "t_a_idx")
	require.NoError(t, err)
	require.True(t, found)

	swap := testutil.NewHookedClock(func() {
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`
			ALTER INDEX %[1]s.t_a_idx RENAME TO t_a_idx_aside;
			CREATE INDEX t_a_idx ON %[1]s.t (b)`, schema))
		require.NoError(t, err)
	})
	_, err = executeDropIndexConcurrently(t.Context(), pool, schema, "t", "t_a_idx", reviewed.Definition, nil,
		ConcurrentBudget{Overall: time.Minute}, swap)
	require.ErrorIs(t, err, ErrDroppedIndexReplaced)
	assert.Equal(t, CodeDroppedIndexReplaced, OutcomeCode(err))
	assert.True(t, OutcomeCode(err).Permanent())

	var survivor string
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT relname FROM pg_class WHERE oid = $1`, checked).Scan(&survivor))
	assert.Equal(t, "t_a_idx_aside", survivor, "the checked index survives under the name it was moved to")
	var named int
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relname = 't_a_idx'`,
		schema).Scan(&named))
	assert.Zero(t, named, "the index that took the name is the one dropped")
}

// afterDropClock cancels the drop's context on its second reading, which
// executeDropIndexConcurrently takes right after DROP INDEX CONCURRENTLY
// returns and before the lookup that confirms which index it dropped.
type afterDropClock struct {
	readings int
	cancel   context.CancelFunc
}

func (c *afterDropClock) Now() time.Time {
	c.readings++
	if c.readings == 2 {
		c.cancel()
	}
	return time.Now()
}

// The caller's context ends right after the drop completes, so the lookup
// that confirms the reviewed index is the one dropped cannot run. The
// executor reports drop-outcome-unknown, naming the checked index's OID,
// rather than a retryable execution failure: the index is already gone.
func TestDropIndexConcurrentlyReportsAnUnconfirmedDropAsOutcomeUnknown(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.t (id int PRIMARY KEY, a int);
		CREATE INDEX t_a_idx ON %[1]s.t (a)`, schema))
	require.NoError(t, err)
	var checked uint32
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT $1::regclass::oid`, schema+".t_a_idx").Scan(&checked))
	reviewed, found, err := schemadiff.ReadDroppedObject(t.Context(), pool, schema, "t", schemadiff.DroppedIndex, "t_a_idx")
	require.NoError(t, err)
	require.True(t, found)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err = executeDropIndexConcurrently(ctx, pool, schema, "t", "t_a_idx", reviewed.Definition, nil,
		ConcurrentBudget{Overall: time.Minute}, &afterDropClock{cancel: cancel})
	unknown, ok := errors.AsType[*DropOutcomeUnknownError](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, checked, unknown.IndexOID)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, CodeDropOutcomeUnknown, OutcomeCode(err))
	assert.False(t, OutcomeCode(err).Permanent())

	var remaining int
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_class WHERE oid = $1`, checked).Scan(&remaining))
	assert.Zero(t, remaining, "the drop itself completed")
}
