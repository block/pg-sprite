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
		CauseShadowOwner,
		CauseForeignRelation,
		CauseGrantsDiffer,
		CauseIdentityHandoff,
	}
}

// Invariant is the identifier, from docs/invariants.md, of the invariant
// the cause upholds.
func (c RefusalCause) Invariant() string {
	switch c {
	case CauseLockUnproven, CauseLockLost, CauseLockHeldElsewhere, CauseLockUnconfirmed:
		return "LK-1"
	case CauseProofEmpty, CauseSourceShape:
		return "ST-6"
	case CauseStatementTarget:
		return "ST-7"
	case CauseShadowOwner, CauseForeignRelation, CauseGrantsDiffer, CauseIdentityHandoff:
		return "ST-5"
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
