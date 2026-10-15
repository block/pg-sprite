package schemachange

import (
	"github.com/block/pg-sprite/pkg/copier"
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
