package schemachange

import (
	"errors"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/internal/chunksql"
)

// copierCause is the shadow refusal cause the Runner re-mints a copier
// refusal cause as, so a caller of Runner.Run reads one vocabulary —
// RefusalCauseOf, errors.Is against ErrInvariantViolation — whether the
// refusal came from a shadow operation or from the copy. It is total over
// copier.RefusalCauses(), and the test beside it pins that; the empty cause
// answers for a cause it does not know, the empty copier cause included.
func copierCause(cause copier.RefusalCause) RefusalCause {
	switch cause {
	case copier.CauseProofEmpty:
		return CauseProofEmpty
	case copier.CauseShadowMismatch:
		return CauseCopyShadowMismatch
	case copier.CauseLockUnproven:
		return CauseLockUnproven
	case copier.CauseLockLost:
		return CauseLockLost
	case copier.CauseLockHeldElsewhere:
		return CauseLockHeldElsewhere
	case copier.CauseLockUnconfirmed:
		return CauseLockUnconfirmed
	case copier.CauseRelationReplaced:
		return CauseRelationReplaced
	case copier.CauseConversionDrift:
		return CauseCopyConversionDrift
	case copier.CauseFrontierBroken:
		return CauseCopyFrontierBroken
	case copier.CauseChunksInFlight:
		return CauseCopyChunksInFlight
	case copier.CauseChunkUnknown:
		return CauseCopyChunkUnknown
	default:
		return ""
	}
}

// copyRefusal re-mints a copier refusal as the Runner's own, wrapping the
// original so errors.Is against the copier's sentinels and causes, and
// against whatever the copier wrapped — the lock session's loss, the
// server's error — keeps working. Any other error, nil included, is
// returned unchanged: a row the shadow rejected or a connection that
// dropped is the server's answer, not a refusal.
func copyRefusal(err error, schema, table string) error {
	cause := copierCause(copier.RefusalCauseOf(err))
	if cause == "" {
		return err
	}
	return refuse(cause, []error{err}, "copy of %s.%s refused", schema, table)
}

// driftRefusal re-mints conversion drift the verifier or the flusher
// noticed — a bare invariant violation wrapping chunksql.ErrConversionDrift
// — as the Runner's copy-conversion-drift, so a run that stops on a changed
// catalog during verification or catch-up reads the same cause as one the
// copier stopped (CO-9). The original is wrapped, so errors.Is against the
// sentinel and against ErrInvariantViolation keeps working. An error that
// already carries a refusal cause — the copier's re-minted drift among
// them — and every error that is not drift are returned unchanged.
func driftRefusal(err error, schema, table string) error {
	if RefusalCauseOf(err) != "" {
		return err
	}
	if !errors.Is(err, chunksql.ErrConversionDrift) {
		return err
	}
	// INV: CO-9
	return refuse(CauseCopyConversionDrift, []error{err}, "the catalog the conversion expressions of %s.%s resolve in has changed since the shadow was built", schema, table)
}

// copyStopped is the error of a copy whose periodic save failed. The save's
// failure is always part of it, since the run has nothing to resume from
// past the last save; a refusal the copier reached on its own is re-minted
// and joined in front of it, so a caller reads the copy's cause whichever
// side noticed the fault first — a lock lost mid-copy fails the save in
// flight and the chunks in flight at once. A copier that merely stopped on
// the save's cancellation carries that same failure and adds nothing.
func copyStopped(runErr, saveErr error, schema, table string) error {
	if copier.RefusalCauseOf(runErr) == "" {
		return saveErr
	}
	return errors.Join(copyRefusal(runErr, schema, table), saveErr)
}
