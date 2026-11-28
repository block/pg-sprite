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

// A slot holding more unconfirmed WAL than the ceiling is refused, the
// refusal naming the slot, the backlog, and the ceiling; one at or within
// the ceiling is admitted, since the catch-up's first reading admits it too.
func TestCheckResumeBacklogRefusesOnlyASlotOverTheCeiling(t *testing.T) {
	err := checkResumeBacklog(unconfirmed(backlogTestCeiling+1), backlogTestCeiling)
	require.Error(t, err)
	assert.Equal(t, CauseResumeSlotBacklog, RefusalCauseOf(err))
	assert.Contains(t, err.Error(), "slot pgsprite_0123abcd holds 1073741825 bytes of WAL its stream has not confirmed, over the 1073741824-byte SlotLagCeiling")

	assert.NoError(t, checkResumeBacklog(unconfirmed(backlogTestCeiling), backlogTestCeiling))
	assert.NoError(t, checkResumeBacklog(unconfirmed(0), backlogTestCeiling))
}

// An unconfirmed measure the server could not take proves no backlog, so
// the resume is not refused for it; the catch-up's first reading fails
// closed on the unknown measure instead.
func TestCheckResumeBacklogLeavesAnUnknownMeasureToTheCatchup(t *testing.T) {
	status := decode.SlotStatus{Name: "pgsprite_0123abcd", Own: true}
	assert.NoError(t, checkResumeBacklog(status, backlogTestCeiling))
}
