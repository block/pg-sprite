package schemachange

import (
	"errors"
	"fmt"
	"strings"
)

// RefusalCause identifies why a shadow operation refused to proceed. Every
// cause is a fail-closed refusal under ErrInvariantViolation; the cause is
// what tells an importer which way to react — re-acquire the lock, clean up
// a relation the engine does not own, drop and rebuild, or report a bug —
// without matching on the message text, which carries schema, role,
// sequence, and backend names an importer must not render verbatim.
type RefusalCause string

const (
	// CauseLockUnproven means the operation was handed no table lock
	// session, or one whose proof is empty or names a different table
	// (LK-1). It is a caller error, refused before any connection opens.
	CauseLockUnproven RefusalCause = "shadow-lock-unproven"
	// CauseLockLost means the table lock session reported that it lost the
	// lock, either before the operation began or while it ran (LK-1). The
	// operation's transaction is aborted; the caller re-acquires the lock
	// and repeats the operation.
	CauseLockLost RefusalCause = "shadow-lock-lost"
	// CauseLockHeldElsewhere means pg_locks shows the table lock granted to
	// a backend other than the lock session's own (LK-1): another engine
	// instance holds the table, and the session this operation trusts does
	// not.
	CauseLockHeldElsewhere RefusalCause = "shadow-lock-held-elsewhere"
	// CauseLockUnconfirmed means pg_locks shows no session holding the
	// table lock, although the lock session has not reported loss (LK-1).
	CauseLockUnconfirmed RefusalCause = "shadow-lock-unconfirmed"
	// CauseProofEmpty means the copy-and-swap target proof is the zero value
	// (ST-6): only the shape check mints a populated one.
	CauseProofEmpty RefusalCause = "shadow-proof-empty"
	// CauseSourceShape means the source's catalog is outside the shape the
	// proof admits in a way the shape check does not decide: an identity
	// column without an internally owned sequence, or a replica identity
	// other than DEFAULT or FULL (ST-6).
	CauseSourceShape RefusalCause = "shadow-source-shape"
	// CauseStatementTarget means the gated statement is not an ALTER TABLE
	// on the proven table, or its retargeted form does not match it in
	// every operation and name the shadow (ST-7).
	CauseStatementTarget RefusalCause = "shadow-statement-target"
	// CauseStatementConversion means the gated statement retypes a column
	// with a USING expression the copy cannot carry as written (ST-7): the
	// deparser cannot prove it rendered the expression faithfully, the
	// expression reads a column other than the one it retypes, the
	// statement retypes the same column twice, the expression converts
	// the primary key, whose values the copy's chunk ranges, watermark, and
	// resume clear are expressed in, or the server does not classify the
	// expression as immutable, so its evaluations in the copy, the recopy,
	// and the catch-up need not agree. The statement's own sentinel, or
	// ErrConversionNotImmutable, is wrapped for errors.Is.
	CauseStatementConversion RefusalCause = "shadow-statement-conversion"
	// CauseShadowOwner means the shadow the build just created is not owned
	// by the source's owner (ST-5): the role the proof carries is no longer
	// the owner the catalog reports, so the proof is stale.
	CauseShadowOwner RefusalCause = "shadow-owner-mismatch"
	// CauseForeignRelation means the relation wearing the shadow's name is
	// not a plain table owned by the source's owner (ST-5): it is not a
	// shadow this engine built, and neither inspection nor drop may treat
	// it as one.
	CauseForeignRelation RefusalCause = "shadow-foreign-relation"
	// CauseGrantsDiffer means the shadow's grants still differ from the
	// source's after the build synchronised them (ST-5).
	CauseGrantsDiffer RefusalCause = "shadow-grants-differ"
	// CauseIdentityHandoff means a source identity column the change kept on
	// the shadow does not carry DEFAULT nextval(<source sequence>) there
	// (ST-5): at build, the gated statement altered the column; at
	// inspection, the shadow was altered since the build.
	CauseIdentityHandoff RefusalCause = "shadow-identity-handoff"
	// CauseCutoverUnverified means the cutover gate was handed no usable
	// data-equality proof (CO-1): the built-shadow proof or the verified
	// shadow proof is empty, the two name different relations, or the
	// verified watermark does not cover the whole key space.
	CauseCutoverUnverified RefusalCause = "cutover-unverified"
	// CauseCutoverUndrained means the cutover was handed a verified shadow
	// proof a live pass minted — the shadow equal to the source as of a
	// position the stream had reached, with every change since still in
	// the stream — and no drain to apply those changes under the swap lock
	// (CO-1).
	CauseCutoverUndrained RefusalCause = "cutover-undrained"
	// CauseDrainDeferred means the shadow refused a batch the swap's drain
	// had to apply under the swap lock (CO-6): a unique-key move the
	// changes since the live pass cannot be ordered into the shadow. No
	// copy is left to change the outcome, so the schema change cannot
	// converge and the swap is rolled back.
	CauseDrainDeferred RefusalCause = "cutover-drain-deferred"
	// CauseRelationReplaced means the source or the shadow under its name
	// is no longer the relation the build proved (ST-6): the OID moved, so
	// the proof describes a table that no longer exists.
	CauseRelationReplaced RefusalCause = "cutover-relation-replaced"
	// CauseSchemaDrift means the source's or the shadow's introspected model
	// no longer matches the fingerprint the build recorded (ST-5): a
	// column, constraint, or index was added, dropped, or changed since.
	CauseSchemaDrift RefusalCause = "cutover-schema-drift"
	// CauseFidelityDrift means the source's or the shadow's metadata — owner,
	// grants, policies, comment, storage parameters, replica identity,
	// statistics targets, unvalidated checks, or identity sequence options —
	// no longer matches the snapshot the build recorded (ST-5).
	CauseFidelityDrift RefusalCause = "cutover-fidelity-drift"
	// CauseIndexInvalid means a shadow index has pg_index.indisvalid false
	// (ST-5): swapping it in would put an index the planner never uses
	// under the user's name.
	CauseIndexInvalid RefusalCause = "cutover-index-invalid"
	// CauseNameTaken means a name cutover must assign is already worn
	// (D8): a derived _old name by a relation or statistics object in the
	// schema, or a source dependent's name by a shadow constraint that is
	// not the one being renamed to it.
	CauseNameTaken RefusalCause = "cutover-name-taken"
	// CauseSwapMismatch means the catalog, re-read inside the swap
	// transaction after the renames and handoffs, does not show what the
	// swap set out to produce — the shadow under the source's name, the
	// source under its _old name, a paired dependent under its new name,
	// or the live table's sequences and identities as the source had them
	// (ST-6). The transaction is rolled back and nothing is swapped.
	CauseSwapMismatch RefusalCause = "cutover-swap-mismatch"
	// CauseOutcomeAmbiguous means that after a failed swap attempt the
	// source's name is borne by neither the source nor the shadow (LK-4):
	// the catalog cannot say whether the swap committed, so the engine
	// stops rather than retry or report either outcome.
	CauseOutcomeAmbiguous RefusalCause = "cutover-outcome-ambiguous"
	// CauseResumeTerminal means the checkpoint row handed to a resume is in
	// a terminal phase (ST-4): the run it records is done or failed, so
	// there is nothing to resume or reconcile.
	CauseResumeTerminal RefusalCause = "resume-row-terminal"
	// CauseResumeCutover means the checkpoint row handed to a resume is in
	// the cutover phase (LK-4): a swap was attempted, and whether it
	// committed is read from the catalog by InspectSwapped, never assumed.
	// Neither a clean resume nor a reconcile acts on the row until the
	// caller has that answer; a swap the catalog shows rolled back is a
	// run back before its cutover, and the caller writes the row there.
	CauseResumeCutover RefusalCause = "resume-row-cutover"
	// CauseResumeRowMismatch means the checkpoint row handed to a resume
	// does not describe the proven target and built shadow (ST-2): it is
	// keyed on another table, names another shadow or slot, or carries the
	// fingerprints of another statement.
	CauseResumeRowMismatch RefusalCause = "resume-row-mismatch"
	// CauseResumeRowDecoding means the checkpoint row handed to a resume
	// was written by the other kind of run (ST-3): the quiesced runner was
	// handed a row that names a slot or publication, or is in a phase only
	// a run that decodes WAL enters; or the live runner was handed a row
	// that names no slot. Continuing a decoding row without its slot would
	// leave the slot as nobody's and the changes it carried unapplied, and
	// a quiesced row names no position a stream could start from, so the
	// row is refused either way.
	CauseResumeRowDecoding RefusalCause = "resume-row-decoding"
	// CauseShadowUnrecorded means a relation already bears the target's
	// derived shadow name while no checkpoint row records a run for the
	// target (ST-2). The row is the only record of which statement a shadow
	// embodies; without one the shadow is dropped and the run started over.
	CauseShadowUnrecorded RefusalCause = "shadow-unrecorded"
	// CauseResumeSlotForeign means the slot wearing the row's derived name
	// is not this route's (ST-3): it is a physical slot, or a logical slot
	// of another database. It is neither decoded from nor replaced; an
	// operator renames or drops it.
	CauseResumeSlotForeign RefusalCause = "resume-slot-foreign"
	// CauseResumeSlotHeld means the row's slot is intact but another
	// backend holds it (ST-4), so no stream can be opened on it now. The
	// holder is named; a walsender of this engine's earlier process lets go
	// within wal_sender_timeout of losing its client, and the resume is
	// inspected again after it.
	CauseResumeSlotHeld RefusalCause = "resume-slot-held"
	// CauseCopyShadowMismatch means the shadow the Runner handed its copier
	// does not describe a copy into the proven target's shadow (ST-6): the
	// built-shadow proof is empty, names another table or the source
	// itself, lacks the relation OIDs, or its copy columns do not carry the
	// primary key. It re-mints the copier's copy-shadow-mismatch.
	CauseCopyShadowMismatch RefusalCause = "copy-shadow-mismatch"
	// CauseCopyFrontierBroken means the copier's own accounting of the key
	// space broke (CO-4): a chunk was cut or claimed out of order, or the
	// copy returned without error at a watermark short of the key space.
	// It re-mints the copier's copy-frontier-broken.
	CauseCopyFrontierBroken RefusalCause = "copy-frontier-broken"
	// CauseCopyChunksInFlight means the copier's ledger and its workers
	// disagreed about what landed (LK-3): a worker landed a chunk the
	// ledger had not in flight, or the copy stopped without error with
	// chunks still in flight. It re-mints the copier's
	// copy-chunks-in-flight.
	CauseCopyChunksInFlight RefusalCause = "copy-chunks-in-flight"
	// CauseCopyChunkUnknown means a chunk no chunker cut was fed back to
	// the copier's chunker as if it had been copied (ST-6). It re-mints the
	// copier's copy-chunk-unknown.
	CauseCopyChunkUnknown RefusalCause = "copy-chunk-unknown"
)

// RefusalCauses returns the closed set of shadow refusal causes, so
// documentation and importers can enumerate them instead of maintaining
// their own list.
func RefusalCauses() []RefusalCause {
	return []RefusalCause{
		CauseLockUnproven,
		CauseLockLost,
		CauseLockHeldElsewhere,
		CauseLockUnconfirmed,
		CauseProofEmpty,
		CauseSourceShape,
		CauseStatementTarget,
		CauseStatementConversion,
		CauseShadowOwner,
		CauseForeignRelation,
		CauseGrantsDiffer,
		CauseIdentityHandoff,
		CauseCutoverUnverified,
		CauseCutoverUndrained,
		CauseDrainDeferred,
		CauseRelationReplaced,
		CauseSchemaDrift,
		CauseFidelityDrift,
		CauseIndexInvalid,
		CauseNameTaken,
		CauseSwapMismatch,
		CauseOutcomeAmbiguous,
		CauseResumeTerminal,
		CauseResumeCutover,
		CauseResumeRowMismatch,
		CauseResumeRowDecoding,
		CauseShadowUnrecorded,
		CauseResumeSlotForeign,
		CauseResumeSlotHeld,
		CauseCopyShadowMismatch,
		CauseCopyFrontierBroken,
		CauseCopyChunksInFlight,
		CauseCopyChunkUnknown,
	}
}

// Invariant is the identifier, from docs/invariants.md, of the invariant
// the cause upholds.
func (c RefusalCause) Invariant() string {
	switch c {
	case CauseLockUnproven, CauseLockLost, CauseLockHeldElsewhere, CauseLockUnconfirmed:
		return "LK-1"
	case CauseProofEmpty, CauseSourceShape, CauseRelationReplaced, CauseSwapMismatch,
		CauseCopyShadowMismatch, CauseCopyChunkUnknown:
		return "ST-6"
	case CauseCopyFrontierBroken:
		return "CO-4"
	case CauseCopyChunksInFlight:
		return "LK-3"
	case CauseStatementTarget, CauseStatementConversion:
		return "ST-7"
	case CauseShadowOwner, CauseForeignRelation, CauseGrantsDiffer, CauseIdentityHandoff,
		CauseSchemaDrift, CauseFidelityDrift, CauseIndexInvalid, CauseNameTaken:
		return "ST-5"
	case CauseCutoverUnverified, CauseCutoverUndrained:
		return "CO-1"
	case CauseDrainDeferred:
		return "CO-6"
	case CauseOutcomeAmbiguous, CauseResumeCutover:
		return "LK-4"
	case CauseResumeTerminal, CauseResumeSlotHeld:
		return "ST-4"
	case CauseResumeRowMismatch, CauseShadowUnrecorded:
		return "ST-2"
	case CauseResumeSlotForeign, CauseResumeRowDecoding:
		return "ST-3"
	default:
		return ""
	}
}

// RefusalError is a shadow operation's fail-closed refusal. It wraps
// ErrInvariantViolation, so errors.Is against that sentinel keeps working,
// and any error behind the refusal — the lock session's loss, the statement
// the cancelled context interrupted — so errors.Is against those works too.
// Detail names the catalog fact that decided the refusal; identifiers in it
// appear unquoted, so a renderer embedding it in structured output owns
// escaping it.
type RefusalError struct {
	// Cause is the refusal cause.
	Cause RefusalCause
	// Detail is the catalog or session fact behind the cause.
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

// RefusalCauseOf returns the shadow refusal cause carried by err, or the
// empty cause when err is nil or carries no shadow refusal. Wrappers are
// read through.
func RefusalCauseOf(err error) RefusalCause {
	var refusal *RefusalError
	if errors.As(err, &refusal) {
		return refusal.Cause
	}
	return ""
}
