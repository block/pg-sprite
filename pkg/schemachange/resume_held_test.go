package schemachange

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/block/pg-sprite/pkg/decode"
)

// Only a reading that proves WAL the resumed run's own slot keeps on the
// volume is discounted: a missing slot, another database's slot, a lost
// slot the resume replaces, and a retained figure the server could not
// measure all count nothing, so the disk check falls back to the fresh
// requirement rather than discounting space it cannot see.
func TestCountsRetainedWAL(t *testing.T) {
	own := decode.SlotStatus{
		Own:       true,
		WALStatus: decode.WALStatusReserved,
		Retained:  decode.RetainedWAL{Bytes: 3 << 20, Known: true},
	}
	assert.True(t, countsRetainedWAL(own, true), "the route's own slot with measured WAL is counted")
	assert.False(t, countsRetainedWAL(own, false), "no slot of the name")

	foreign := own
	foreign.Own = false
	assert.False(t, countsRetainedWAL(foreign, true), "another database's or a physical slot")

	lost := own
	lost.WALStatus = decode.WALStatusLost
	assert.False(t, countsRetainedWAL(lost, true), "a lost slot is replaced by the resume")

	unknown := own
	unknown.Retained = decode.RetainedWAL{}
	assert.False(t, countsRetainedWAL(unknown, true), "an unmeasured figure is not read as held WAL")

	unknownWithBytes := own
	unknownWithBytes.Retained = decode.RetainedWAL{Bytes: 3 << 20, Known: false}
	assert.False(t, countsRetainedWAL(unknownWithBytes, true), "a figure the server marked unknown is not held WAL, whatever its bytes say")

	extended := own
	extended.WALStatus = decode.WALStatusExtended
	assert.True(t, countsRetainedWAL(extended, true), "a slot past max_wal_size still holds its WAL")
}
