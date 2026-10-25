package migrate_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/migrate"
)

// The route needs one pool connection per copy worker and two beside them:
// one for the catch-up's flushes, one for checkpoint saves and progress
// reads. Zero workers means the copier's default of four.
func TestCopySwapMinConnsIsTheWorkersAndTwo(t *testing.T) {
	assert.Equal(t, int32(6), migrate.CopySwapMinConns(copier.Options{}))
	assert.Equal(t, int32(3), migrate.CopySwapMinConns(copier.Options{Workers: 1}))
	assert.Equal(t, int32(10), migrate.CopySwapMinConns(copier.Options{Workers: 8}))
}
