package suggest_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/suggest"
)

func parseOps(t *testing.T, sql string) []statement.Op {
	t.Helper()
	ops, err := statement.ParseOps(sql)
	require.NoError(t, err)
	return ops
}

// One ALTER that drops a column and adds it back under the same name is
// the drop-and-re-add shape, in either order.
func TestReAddedColumnFindsADropAndReAdd(t *testing.T) {
	column, ok := suggest.ReAddedColumn(parseOps(t, `
		ALTER TABLE orders
			DROP COLUMN qty,
			ADD COLUMN qty bigint`))
	require.True(t, ok)
	assert.Equal(t, "qty", column)

	column, ok = suggest.ReAddedColumn(parseOps(t, `
		ALTER TABLE orders
			ADD COLUMN note text,
			DROP COLUMN note`))
	require.True(t, ok, "PostgreSQL refuses this order, but the shape is the same and so is the advice")
	assert.Equal(t, "note", column)
}

// A drop with nothing added back, or with a different column added, is
// not the shape.
func TestReAddedColumnIgnoresOtherDrops(t *testing.T) {
	_, ok := suggest.ReAddedColumn(parseOps(t, `ALTER TABLE orders DROP COLUMN qty`))
	assert.False(t, ok)

	_, ok = suggest.ReAddedColumn(parseOps(t, `
		ALTER TABLE orders
			DROP COLUMN qty,
			ADD COLUMN quantity bigint`))
	assert.False(t, ok)
}
