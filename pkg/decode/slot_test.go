package decode_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/preflight"
)

// The zero target is forgeable and names no table; it is refused before any
// connection is made, so a nil pool is never touched.
func TestCreateSlotRefusesTheZeroTargetWithoutConnecting(t *testing.T) {
	slot, err := decode.CreateSlot(t.Context(), dbconn.Config{}, nil, preflight.CopySwapTarget{})
	require.ErrorIs(t, err, decode.ErrInvariantViolation)
	assert.Nil(t, slot)
}

// The drop primitive only ever drops a slot of the engine's own shape —
// the prefix and the eight hex digits preflight derives — so it cannot be
// pointed at anyone else's slot, and refuses before touching the pool.
func TestDropSlotRefusesANameOutsideTheEngineShape(t *testing.T) {
	for _, name := range []string{
		"",
		"orders_slot",
		"pgsprite_",
		"pgsprite_1234567",
		"pgsprite_123456789",
		"pgsprite_ABCDEF01",
		"pgsprite_0123abcd; DROP TABLE x",
		"PGSPRITE_0123abcd",
	} {
		t.Run(name, func(t *testing.T) {
			err := decode.DropSlot(t.Context(), dbconn.Config{}, nil, name)
			require.ErrorIs(t, err, decode.ErrInvariantViolation)
		})
	}
}

func TestSlotExistsErrorNamesTheSlot(t *testing.T) {
	err := &decode.SlotExistsError{Name: "pgsprite_0123abcd"}
	assert.Equal(t, "replication slot pgsprite_0123abcd already exists", err.Error())
}
