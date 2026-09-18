package dbconn

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTableLockQualifiedName(t *testing.T) {
	assert.Equal(t, "odd.schema.table.with.dots", tableLockQualifiedName("odd.schema", "table.with.dots"))
}
