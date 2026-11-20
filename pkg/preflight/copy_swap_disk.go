package preflight

import (
	"fmt"
	"math"
)

// refuseCopySwapDisk decides whether the caller-measured free space covers
// what the run still needs on the volume, or nil when it does: the shadow
// copy's headroom and, for a run that decodes WAL, the slot lag ceiling
// beside it, each less what a stopped run being resumed already holds of
// it (CopySwapEnvironment.Held). An unmeasured volume, a negative held
// figure, and a negative ceiling are refused before any size is compared.
func refuseCopySwapDisk(f copySwapEnvironmentFacts, env CopySwapEnvironment) *CopySwapEnvironmentError {
	if env.FreeDiskBytes <= 0 {
		return &CopySwapEnvironmentError{
			Cause:  CopySwapCauseDiskHeadroom,
			Detail: fmt.Sprintf("free disk space was not measured (%d bytes reported); the volume's free space must be passed in", env.FreeDiskBytes),
		}
	}
	if env.Held.ShadowBytes < 0 || env.Held.RetainedWALBytes < 0 {
		return &CopySwapEnvironmentError{
			Cause: CopySwapCauseDiskHeadroom,
			Detail: fmt.Sprintf("a resumed run was reported to hold %d bytes of shadow and %d bytes of retained WAL; a size the server measured is never negative",
				env.Held.ShadowBytes, env.Held.RetainedWALBytes),
		}
	}
	shadow := f.totalBytes * copySwapDiskHeadroomFactor
	// INV: ST-6 — the shadow a resumed run already built is space the
	// measured free figure already excludes, so only the rest of the
	// headroom must still fit. The discount never exceeds the requirement,
	// so the need left cannot go negative.
	heldShadow := min(shadow, env.Held.ShadowBytes)
	shadowNeed := shadow - heldShadow
	if !env.LogicalDecoding {
		if env.FreeDiskBytes < shadowNeed {
			return &CopySwapEnvironmentError{
				Cause: CopySwapCauseDiskHeadroom,
				Detail: fmt.Sprintf("%d bytes are free; %d bytes (%d× the table's %d bytes of heap, indexes, and TOAST) are required%s",
					env.FreeDiskBytes, shadow, copySwapDiskHeadroomFactor, f.totalBytes, heldClause(heldShadow, 0, shadowNeed)),
				FreeDiskBytes:       env.FreeDiskBytes,
				RequiredShadowBytes: shadow,
				HeldShadowBytes:     heldShadow,
			}
		}
		return nil
	}
	ceiling := env.SlotLagCeilingBytes
	if ceiling < 0 {
		return &CopySwapEnvironmentError{
			Cause:  CopySwapCauseDiskHeadroom,
			Detail: fmt.Sprintf("the slot lag ceiling is %d bytes; a live run must count the ceiling its catch-up enforces, zero for the default", ceiling),
		}
	}
	if ceiling == 0 {
		ceiling = DefaultSlotLagCeiling
	}
	// INV: ST-6 — the WAL a resumed run's slot already retains is on the
	// volume and counts toward the ceiling, so only the rest of the
	// ceiling can still grow; as with the shadow, at most the ceiling.
	heldWAL := min(ceiling, env.Held.RetainedWALBytes)
	walNeed := ceiling - heldWAL
	// The WAL the ceiling lets the slot retain lands on the same volume as
	// the shadow, so a run that decodes WAL needs room for both, and one
	// refusal names the whole requirement even when the shadow alone does
	// not fit. The remainder is compared rather than the sum, which a
	// ceiling near the int64 limit would overflow; free is positive and
	// the shadow's need is not negative, so the remainder cannot wrap.
	if env.FreeDiskBytes-shadowNeed < walNeed {
		return &CopySwapEnvironmentError{
			Cause: CopySwapCauseDiskHeadroom,
			Detail: fmt.Sprintf("%d bytes are free; a live run needs %d bytes (%d× the table's %d bytes of heap, indexes, and TOAST) for the shadow copy plus the slot lag ceiling of %d bytes of WAL its replication slot may retain%s; lower the ceiling or free space on the volume",
				env.FreeDiskBytes, shadow, copySwapDiskHeadroomFactor, f.totalBytes, ceiling, heldClause(heldShadow, heldWAL, remainingNeed(shadowNeed, walNeed))),
			FreeDiskBytes:       env.FreeDiskBytes,
			RequiredShadowBytes: shadow,
			SlotLagCeilingBytes: ceiling,
			HeldShadowBytes:     heldShadow,
			HeldWALBytes:        heldWAL,
		}
	}
	return nil
}

// remainingNeed is the sum of the shadow's and the WAL's remaining needs
// for a refusal's prose, saturated at the int64 limit rather than wrapped.
func remainingNeed(shadowNeed, walNeed int64) int64 {
	if shadowNeed > math.MaxInt64-walNeed {
		return math.MaxInt64
	}
	return shadowNeed + walNeed
}

// heldClause names, in a refusal's prose, what a resumed run already holds
// and what is still needed beyond it; a fresh run holds nothing and gets
// no clause.
func heldClause(heldShadow, heldWAL, need int64) string {
	if heldShadow == 0 && heldWAL == 0 {
		return ""
	}
	return fmt.Sprintf(", less the %d bytes of shadow and %d bytes of retained WAL the stopped run being resumed already holds on the volume, so %d bytes more are needed",
		heldShadow, heldWAL, need)
}
