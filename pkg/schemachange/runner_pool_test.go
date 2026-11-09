package schemachange_test

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// A run needs one pool connection per copy worker and two beside them:
// one for the catch-up's flushes, one for checkpoint saves and progress
// reads. Zero workers means the copier's default of four.
func TestMinPoolConnsIsTheWorkersAndTwo(t *testing.T) {
	assert.Equal(t, int32(6), schemachange.MinPoolConns(copier.Options{}))
	assert.Equal(t, int32(3), schemachange.MinPoolConns(copier.Options{Workers: 1}))
	assert.Equal(t, int32(10), schemachange.MinPoolConns(copier.Options{Workers: 8}))
	assert.Equal(t, int32(math.MaxInt32), schemachange.MinPoolConns(copier.Options{Workers: math.MaxInt32}),
		"a worker count no pool can hold is not wrapped to one every pool passes")
}
