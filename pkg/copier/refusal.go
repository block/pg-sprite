package copier

import (
	"errors"
	"fmt"
	"strings"

	"github.com/block/pg-sprite/pkg/refusal"
)

// RefusalCause identifies why the copier refused to copy, or refused to
// report a copy as complete. Every cause is a fail-closed refusal under
// ErrInvariantViolation; the cause is what tells the orchestrator that runs
// the copy which way to react — re-acquire the lock, drop the shadow and
// start the change over, or report a bug — without matching on the message
// text, which carries schema, table, and backend names.
type RefusalCause string

const (
	// CauseProofEmpty means the copy-and-swap target proof is the zero
	// value (ST-6): only the preflight checks mint a populated one.
	CauseProofEmpty RefusalCause = "copy-proof-empty"
	// CauseShadowMismatch means the shadow handed to the copier does not
	// describe a copy into the proven target's shadow (ST-6): it is nil or
	// empty, built for another table, the source itself by name or by
	// relation, without the OIDs the per-chunk relation check compares
	// against, or its copy columns do not carry the primary key.
	CauseShadowMismatch RefusalCause = "copy-shadow-mismatch"
	// CauseLockUnproven means the copier was handed no table lock session,
	// or one whose proof is empty or names a different table (LK-1). It is
	// a caller error, refused before any connection opens.
	CauseLockUnproven RefusalCause = "copy-lock-unproven"
	// CauseLockLost means the table lock session reported that it lost the
	// lock, before the copy began or while it ran (LK-1). The chunk the
	// loss interrupted stays in flight; the caller re-acquires the lock and
	// resumes from the landed watermark.
	CauseLockLost RefusalCause = "copy-lock-lost"
	// CauseLockHeldElsewhere means pg_locks, read from the connection about
	// to write a chunk, shows the table lock granted to a backend other
	// than the lock session's own (LK-1): another engine instance holds the
	// table, and the session this copy trusts does not.
	CauseLockHeldElsewhere RefusalCause = "copy-lock-held-elsewhere"
	// CauseLockUnconfirmed means pg_locks, read from the connection about
	// to write a chunk, shows no session holding the table lock, although
	// the lock session has not reported loss (LK-1).
	CauseLockUnconfirmed RefusalCause = "copy-lock-unconfirmed"
	// CauseRelationReplaced means the source or the shadow is no longer the
	// relation its proof was minted for (ST-6): the name no longer
	// resolves, resolves to another OID, or the key column the proof says
	// is a primary key answered a boundary query with NULL.
	CauseRelationReplaced RefusalCause = "copy-relation-replaced"
	// CauseConversionDrift means the catalog entries the conversion
	// expressions resolve among are no longer the ones the shadow build
	// saw (CO-9): a function, operator, type, or collation under a name an
	// expression spells was created, replaced, or dropped in a schema on
	// the copy's search_path, so a chunk written now could evaluate the
	// expressions differently from the gated statement. The chunk is not
	// written; the caller drops the shadow and starts the change over.
	CauseConversionDrift RefusalCause = "copy-conversion-drift"
	// CauseFrontierBroken means the copier's own accounting of the key
	// space broke (CO-4): a chunk was cut with its lower bound above its
	// upper, a claimed chunk does not start at the cut frontier, or the
	// copy stopped without error short of the key space.
	CauseFrontierBroken RefusalCause = "copy-frontier-broken"
	// CauseChunksInFlight means the copy's ledger and its workers disagree
	// about what landed (LK-3): a worker landed a chunk the ledger had not
	// in flight, or the copy stopped without error with chunks still in
	// flight.
	CauseChunksInFlight RefusalCause = "copy-chunks-in-flight"
	// CauseChunkUnknown means a chunk no chunker cut — a zero Chunk, or one
	// built with NewChunk — was fed back to the chunker as if it had been
	// copied (ST-6).
	CauseChunkUnknown RefusalCause = "copy-chunk-unknown"
)

// RefusalCauses returns the closed set of copier refusal causes, so
// documentation and the orchestrator can enumerate them instead of
// maintaining their own list.
func RefusalCauses() []RefusalCause {
	return []RefusalCause{
		CauseProofEmpty,
		CauseShadowMismatch,
		CauseLockUnproven,
		CauseLockLost,
		CauseLockHeldElsewhere,
		CauseLockUnconfirmed,
		CauseRelationReplaced,
		CauseConversionDrift,
		CauseFrontierBroken,
		CauseChunksInFlight,
		CauseChunkUnknown,
	}
}

// Invariant is the identifier, from docs/invariants.md, of the invariant
// the cause upholds.
func (c RefusalCause) Invariant() string {
	switch c {
	case CauseProofEmpty, CauseShadowMismatch, CauseRelationReplaced, CauseChunkUnknown:
		return "ST-6"
	case CauseLockUnproven, CauseLockLost, CauseLockHeldElsewhere, CauseLockUnconfirmed:
		return "LK-1"
	case CauseConversionDrift:
		return "CO-9"
	case CauseFrontierBroken:
		return "CO-4"
	case CauseChunksInFlight:
		return "LK-3"
	default:
		return ""
	}
}

// Class is the routing class of the cause, from docs/refusal-classes.md:
// what kind of boundary the importer is looking at. A lock the session lost
// or that the server shows held elsewhere or by nobody, a relation that is
// no longer the one proved, and a catalog the conversion expressions no
// longer resolve in as the build did, are environmental — state outside
// the copier moved under it. Every other cause is an invariant violation:
// a proof, shadow, or chunk the caller handed in that cannot have come
// from this engine, or a ledger that disagrees with its workers; nothing
// on the server changes that, so the importer reports it. The class is a
// triage decision; the cause's row in the document decides what the
// importer does next, and no class licenses an automatic retry on its own.
// An unknown cause has no class.
func (c RefusalCause) Class() refusal.Class {
	switch c {
	case CauseLockLost, CauseLockHeldElsewhere, CauseLockUnconfirmed, CauseRelationReplaced, CauseConversionDrift:
		return refusal.ClassEnvironmental
	case CauseProofEmpty, CauseShadowMismatch, CauseLockUnproven, CauseFrontierBroken,
		CauseChunksInFlight, CauseChunkUnknown:
		return refusal.ClassInvariantViolation
	default:
		return ""
	}
}

// RefusalError is the copier's fail-closed refusal. It wraps
// ErrInvariantViolation, so errors.Is against that sentinel keeps working,
// and any error behind the refusal — the lock session's loss, the server's
// word that the table is not held, the statement that found a relation
// gone — so errors.Is and errors.As against those work too. Detail names
// the fact that decided the refusal; identifiers in it appear unquoted, so
// a renderer embedding it in structured output owns escaping it.
type RefusalError struct {
	// Cause is the refusal cause.
	Cause RefusalCause
	// Detail is the ledger, session, or catalog fact behind the cause.
	Detail  string
	wrapped []error
}

// Error implements the error interface.
func (e *RefusalError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s (%s): %s", ErrInvariantViolation, e.Cause.Invariant(), e.Cause, e.Detail)
	for _, err := range e.wrapped {
		b.WriteString(": ")
		b.WriteString(err.Error())
	}
	return b.String()
}

// Unwrap exposes ErrInvariantViolation and every wrapped error to errors.Is
// and errors.As.
func (e *RefusalError) Unwrap() []error {
	return append([]error{ErrInvariantViolation}, e.wrapped...)
}

// refuse mints the refusal for cause, with detail as a format string and
// wrapped as the errors behind it.
func refuse(cause RefusalCause, wrapped []error, detail string, args ...any) error {
	return &RefusalError{Cause: cause, Detail: fmt.Sprintf(detail, args...), wrapped: wrapped}
}

// RefusalCauseOf returns the copier refusal cause carried by err, or the
// empty cause when err is nil or carries no copier refusal. Wrappers are
// read through. RefusalCauseOf(err).Class() is therefore the refusal's
// class, and the empty class for an error that is not a copier refusal — a
// server error the importer routes by SQLSTATE, or the bare sentinel.
func RefusalCauseOf(err error) RefusalCause {
	var refusal *RefusalError
	if errors.As(err, &refusal) {
		return refusal.Cause
	}
	return ""
}
