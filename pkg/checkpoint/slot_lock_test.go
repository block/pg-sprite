package checkpoint

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// The slot lock and the table lock are two advisory keys even for names
// whose hashes collide: their class halves differ.
func TestSlotLockClassIsNotTheTableLockClass(t *testing.T) {
	assert.NotEqual(t, dbconn.TableLockClassID, slotLockClassID)
}
