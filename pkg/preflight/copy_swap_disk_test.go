package preflight

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A live run that resumes a stopped one is asked only for what is left to
// grow: the shadow's headroom less the shadow already built, and the
// ceiling less the WAL the slot already retains. Exactly that remainder
// is admitted; a byte less is refused with the gross figures and both
// discounts on the refusal, so the remainder is derivable from typed
// fields alone.
func TestRefuseCopySwapDiskDiscountsWhatAResumedRunHolds(t *testing.T) {
	facts, env := sufficientCopySwapEnvironment()
	shadow := facts.totalBytes * copySwapDiskHeadroomFactor
	env.SlotLagCeilingBytes = 4 << 20
	env.Held = CopySwapHeld{ShadowBytes: 768 << 10, RetainedWALBytes: 3 << 20}
	remaining := shadow - 768<<10 + 4<<20 - 3<<20

	env.FreeDiskBytes = remaining
	assert.Nil(t, refuseCopySwapDisk(facts, env), "exactly what is left to grow is admitted")

	env.FreeDiskBytes = remaining - 1
	refusal := refuseCopySwapDisk(facts, env)
	require.NotNil(t, refusal)
	assert.Equal(t, CopySwapCauseDiskHeadroom, refusal.Cause)
	assert.Equal(t, remaining-1, refusal.FreeDiskBytes)
	assert.Equal(t, shadow, refusal.RequiredShadowBytes)
	assert.Equal(t, int64(4<<20), refusal.SlotLagCeilingBytes)
	assert.Equal(t, int64(768<<10), refusal.HeldShadowBytes)
	assert.Equal(t, int64(3<<20), refusal.HeldWALBytes)
	assert.Equal(t, remaining, refusal.RequiredShadowBytes-refusal.HeldShadowBytes+refusal.SlotLagCeilingBytes-refusal.HeldWALBytes)

	env.Held = CopySwapHeld{}
	refusal = refuseCopySwapDisk(facts, env)
	require.NotNil(t, refusal, "a fresh run at the same figure needs the whole requirement")
	assert.Zero(t, refusal.HeldShadowBytes)
	assert.Zero(t, refusal.HeldWALBytes)
}

// Each discount is capped at the requirement it is taken from, so a shadow
// larger than its headroom or a slot already past the ceiling cannot pay
// for the other requirement: a run holding more WAL than the ceiling
// still needs the rest of the shadow's headroom, and one holding a shadow
// larger than the headroom still needs the rest of the ceiling.
func TestRefuseCopySwapDiskCapsEachDiscountAtItsRequirement(t *testing.T) {
	facts, env := sufficientCopySwapEnvironment()
	shadow := facts.totalBytes * copySwapDiskHeadroomFactor
	env.SlotLagCeilingBytes = 1 << 20

	env.Held = CopySwapHeld{ShadowBytes: 0, RetainedWALBytes: 5 << 20}
	env.FreeDiskBytes = shadow - 1
	refusal := refuseCopySwapDisk(facts, env)
	require.NotNil(t, refusal, "WAL past the ceiling does not pay for the shadow")
	assert.Equal(t, int64(1<<20), refusal.HeldWALBytes, "the WAL discount is capped at the ceiling")
	env.FreeDiskBytes = shadow
	assert.Nil(t, refuseCopySwapDisk(facts, env))

	env.Held = CopySwapHeld{ShadowBytes: 3 * shadow, RetainedWALBytes: 0}
	env.FreeDiskBytes = 1<<20 - 1
	refusal = refuseCopySwapDisk(facts, env)
	require.NotNil(t, refusal, "a shadow past its headroom does not pay for the ceiling")
	assert.Equal(t, shadow, refusal.HeldShadowBytes, "the shadow discount is capped at the headroom")
	env.FreeDiskBytes = 1 << 20
	assert.Nil(t, refuseCopySwapDisk(facts, env))
}

// The comparison stays overflow-safe with the discounts: a ceiling at the
// int64 limit is still refused on a volume that cannot hold it, and a slot
// that already retains that much leaves nothing of the ceiling to grow.
func TestRefuseCopySwapDiskDiscountsWithoutOverflow(t *testing.T) {
	facts, env := sufficientCopySwapEnvironment()
	shadow := facts.totalBytes * copySwapDiskHeadroomFactor
	env.SlotLagCeilingBytes = math.MaxInt64
	env.FreeDiskBytes = math.MaxInt64
	env.Held = CopySwapHeld{ShadowBytes: shadow / 2}

	refusal := refuseCopySwapDisk(facts, env)
	require.NotNil(t, refusal, "a ceiling the volume cannot hold beside the rest of the shadow is refused, not overflowed")
	assert.Equal(t, int64(math.MaxInt64), refusal.SlotLagCeilingBytes)
	assert.Equal(t, shadow/2, refusal.HeldShadowBytes)
	assert.Zero(t, refusal.HeldWALBytes)

	env.Held.RetainedWALBytes = math.MaxInt64
	env.FreeDiskBytes = shadow - shadow/2
	assert.Nil(t, refuseCopySwapDisk(facts, env), "a slot that retains the whole ceiling has nothing left of it to grow")
}

// A quiesced run holds no slot, so only its shadow is discounted, and a
// retained figure handed to it counts for nothing.
func TestRefuseCopySwapDiskDiscountsOnlyTheShadowForAQuiescedRun(t *testing.T) {
	facts, env := sufficientCopySwapEnvironment()
	shadow := facts.totalBytes * copySwapDiskHeadroomFactor
	env.LogicalDecoding = false
	env.Held = CopySwapHeld{ShadowBytes: 512 << 10, RetainedWALBytes: 1 << 30}

	env.FreeDiskBytes = shadow - 512<<10
	assert.Nil(t, refuseCopySwapDisk(facts, env))

	env.FreeDiskBytes--
	refusal := refuseCopySwapDisk(facts, env)
	require.NotNil(t, refusal)
	assert.Equal(t, shadow, refusal.RequiredShadowBytes)
	assert.Equal(t, int64(512<<10), refusal.HeldShadowBytes)
	assert.Zero(t, refusal.SlotLagCeilingBytes)
	assert.Zero(t, refusal.HeldWALBytes, "an offline run holds no slot")
}

// A held figure no server measurement produces is refused before any size
// is compared, rather than read as extra free space.
func TestRefuseCopySwapDiskRefusesANegativeHeldFigure(t *testing.T) {
	for name, held := range map[string]CopySwapHeld{
		"shadow": {ShadowBytes: -1},
		"wal":    {RetainedWALBytes: -1},
	} {
		t.Run(name, func(t *testing.T) {
			facts, env := sufficientCopySwapEnvironment()
			env.FreeDiskBytes = math.MaxInt64
			env.Held = held
			refusal := refuseCopySwapDisk(facts, env)
			require.NotNil(t, refusal)
			assert.Equal(t, CopySwapCauseDiskHeadroom, refusal.Cause)
			assert.Zero(t, refusal.FreeDiskBytes, "a refused held figure is not a measured disk refusal")
		})
	}
}

// A run whose stopped run's swap has committed only drops the slot and the
// retained source, so it is asked for neither the shadow's headroom nor
// the slot lag ceiling: one free byte admits it, live or quiesced, while a
// run that still copies is refused at the same figure. The volume must
// still have been measured, and a negative ceiling is still a wiring
// error.
func TestRefuseCopySwapDiskAsksNothingOfACommittedSwap(t *testing.T) {
	for name, decodes := range map[string]bool{"live": true, "quiesced": false} {
		t.Run(name, func(t *testing.T) {
			facts, env := sufficientCopySwapEnvironment()
			env.LogicalDecoding = decodes
			env.FreeDiskBytes = 1
			require.NotNil(t, refuseCopySwapDisk(facts, env), "a run that still copies needs the copy's headroom")

			env.Held = CopySwapHeld{SwapCommitted: true}
			assert.Nil(t, refuseCopySwapDisk(facts, env), "finishing a committed swap grows nothing")

			env.FreeDiskBytes = 0
			refusal := refuseCopySwapDisk(facts, env)
			require.NotNil(t, refusal, "an unmeasured volume is refused whatever the run has left")
			assert.Equal(t, CopySwapCauseDiskHeadroom, refusal.Cause)
		})
	}

	facts, env := sufficientCopySwapEnvironment()
	env.FreeDiskBytes = 1
	env.SlotLagCeilingBytes = -1
	env.Held = CopySwapHeld{SwapCommitted: true}
	refusal := refuseCopySwapDisk(facts, env)
	require.NotNil(t, refusal, "a negative ceiling is refused whatever the run has left")
	assert.Equal(t, CopySwapCauseDiskHeadroom, refusal.Cause)

	env.LogicalDecoding = false
	assert.Nil(t, refuseCopySwapDisk(facts, env), "a quiesced run counts no ceiling, so its value is not judged")
}
