package schemachange_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// The resume's shadow size read runs on the caller's pool, whatever its
// search_path (CO-9). A pg_total_relation_size(oid) planted on a
// shadowing <schema>, pg_catalog path matches the read's oid argument
// exactly, so it outranks the catalog's regclass signature, which needs a
// cast: an unqualified call would answer 0 and the disk check would
// discount nothing for a shadow that stands. The qualified read still
// reports the shadow's real size.
func TestMeasureResumeHeldReadsTheRealShadowSizeUnderAPlantedSizeFunction(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	shadow := f.leaveCopyingRow(t, func(*checkpoint.Checkpoint) {})
	shape := f.copySwapShape(t)
	want := f.totalRelationSize(t, shadow)

	planted := testutil.NewSchema(t, f.pool)
	_, err := f.pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE FUNCTION %s.pg_total_relation_size(oid) RETURNS bigint
		LANGUAGE sql AS 'SELECT 0::bigint'`, planted))
	require.NoError(t, err)
	pool := testutil.NewCatalogShadowingPool(t, f.cfg.URL, planted)
	var answered int64
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT pg_total_relation_size(c.oid)
		FROM pg_catalog.pg_class c
		WHERE c.oid OPERATOR(pg_catalog.=) $1::pg_catalog.regclass`,
		pgx.Identifier{f.schema, shadow}.Sanitize()).Scan(&answered))
	require.Zero(t, answered, "the planted function must answer an unqualified call on this pool")

	held, err := schemachange.MeasureResumeHeld(t.Context(), pool, f.store, shape, false)
	require.NoError(t, err)
	assert.Equal(t, preflight.CopySwapHeld{ShadowBytes: want}, held)
}

// The read finds the shadow by comparing names. A lookalike name = name
// that answers true on a shadowing <schema>, pg_catalog path would match
// every ordinary table of every schema and size whichever one the server
// returned first. The read spells its comparisons
// OPERATOR(pg_catalog.=), so it still sizes the shadow alone.
func TestMeasureResumeHeldSizesTheShadowAloneUnderALookalikeNameEquals(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	shadow := f.leaveCopyingRow(t, func(*checkpoint.Checkpoint) {})
	shape := f.copySwapShape(t)
	want := f.totalRelationSize(t, shadow)
	pool := testutil.NewLookalikeNameMatchPool(t, f.cfg.URL, f.pool)

	held, err := schemachange.MeasureResumeHeld(t.Context(), pool, f.store, shape, false)
	require.NoError(t, err)
	assert.Equal(t, preflight.CopySwapHeld{ShadowBytes: want}, held)
}
