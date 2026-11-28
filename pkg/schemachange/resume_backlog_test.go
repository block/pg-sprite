package schemachange

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/decode"
)

// backlogTestCeiling is the slot lag ceiling the backlog check is judged
// against here.
const backlogTestCeiling = 1 << 30

func unconfirmed(bytes int64) decode.SlotStatus {
	return decode.SlotStatus{Name: "pgsprite_0123abcd", Own: true, Unconfirmed: decode.RetainedWAL{Bytes: bytes, Known: true}}
}

// A slot holding more unconfirmed WAL than the ceiling is refused, and the
// refusal carries the slot, the backlog, and the ceiling as a typed value
// a host reads with errors.As; one at or within the ceiling is admitted,
// since the catch-up's first reading admits it too.
func TestCheckResumeBacklogRefusesOnlyASlotOverTheCeiling(t *testing.T) {
	err := checkResumeBacklog(unconfirmed(backlogTestCeiling+1), backlogTestCeiling)
	require.Error(t, err)
	assert.Equal(t, CauseResumeSlotBacklog, RefusalCauseOf(err))
	var backlog *SlotBacklogError
	require.ErrorAs(t, err, &backlog)
	assert.Equal(t, SlotBacklogError{Slot: "pgsprite_0123abcd", Unconfirmed: backlogTestCeiling + 1, Ceiling: backlogTestCeiling}, *backlog)
	assert.NotErrorIs(t, err, decode.ErrSlotLagCeiling, "a refused resume is not the catch-up's stop")

	assert.NoError(t, checkResumeBacklog(unconfirmed(backlogTestCeiling), backlogTestCeiling))
	assert.NoError(t, checkResumeBacklog(unconfirmed(0), backlogTestCeiling))
}

// An unconfirmed measure the server could not take proves no backlog, so
// the resume is not refused for it, whatever byte count the status
// carries beside it; the catch-up's first reading fails closed on the
// unknown measure instead.
func TestCheckResumeBacklogLeavesAnUnknownMeasureToTheCatchup(t *testing.T) {
	status := decode.SlotStatus{Name: "pgsprite_0123abcd", Own: true,
		Unconfirmed: decode.RetainedWAL{Bytes: backlogTestCeiling + 1, Known: false}}
	assert.NoError(t, checkResumeBacklog(status, backlogTestCeiling))
}
