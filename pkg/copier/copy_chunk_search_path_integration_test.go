package copier_test

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/copier"
)

// Every chunk transaction pins the catalog alone on its search_path (CO-9),
// so the copy statement's key range resolves BETWEEN to the catalog's
// operators whatever the caller's pool puts ahead of pg_catalog. The schema
// first on this pool's path offers a bigint <= that is never true; under the
// session's own path every chunk's range would be empty, and the copy
// would land nothing and still report the whole key space as copied.
// Functions in the statement are not at stake — it names none — so the
// operator is the one name only the transaction's search_path can pin.
func TestCopierIgnoresTheSessionSearchPath(t *testing.T) {
	f := newCopierFixture(t)
	const rows = 300
	target, lock, shadow := f.prepare(t, rows)
	f.exec(t, `CREATE FUNCTION %s.never_le(bigint, bigint) RETURNS boolean LANGUAGE sql IMMUTABLE AS 'SELECT false'`)
	f.exec(t, `CREATE OPERATOR %s.<= (LEFTARG = bigint, RIGHTARG = bigint, FUNCTION = %s.never_le)`)
	shadowing := testutil.NewCatalogShadowingPool(t, f.cfg.URL, f.schema)

	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{
		Workers: 2,
		Chunker: copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
	})
	require.NoError(t, err)
	require.NoError(t, c.Run(t.Context(), shadowing))

	f.assertConverged(t, shadow)
	assert.Equal(t, int64(rows), c.Position().RowsInserted, "every row landed through the catalog's operators")
	assert.Equal(t, copier.NewWatermark(math.MaxInt64), c.Position().Watermark)
}
