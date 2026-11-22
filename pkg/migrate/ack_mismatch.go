package migrate

import (
	"fmt"

	"github.com/block/pg-sprite/pkg/statement"
)

// AcknowledgementMismatchError is the usage error [Run] returns when a typed
// acknowledgement names a table other than the one it must name. Nothing has
// executed. It unwraps to the acknowledgement's own sentinel —
// [ErrForceMismatch], [ErrAcceptBlockingMismatch],
// [ErrAllowDestructiveMismatch], [ErrRestartMismatch], or
// [ErrBlockWritesMismatch] — so errors.Is answers that sentinel and no
// other. An adapter that asks whether an error is any wrong-table
// acknowledgement uses errors.As once, and one that shows the operator what
// they gave and what the target is reads the fields rather than the message.
//
// The engine builds it; a value built elsewhere, such as in an adapter's
// tests, behaves the same only when Acknowledgement is spelled exactly as
// [Acknowledgements] returns it, and for "accept-blocking" sets
// LockingStatement.
type AcknowledgementMismatchError struct {
	// Acknowledgement is the acknowledgement's name as [Acknowledgements]
	// spells it, such as "force".
	Acknowledgement string
	// Table is the table the acknowledgement must name — the resolved
	// target, or for "accept-blocking" the table the statement locks — in
	// the engine's flat SCHEMA.TABLE spelling: an unquoted name folded to
	// lower case, a quoted one kept as written, with no quoting added, so it
	// is what the operator must type and cannot be split back into schema
	// and table. It is empty for a statement that names no table, which no
	// acknowledgement value matches.
	Table string
	// Given is the value the acknowledgement carried.
	Given string
	// LockingStatement is, for "accept-blocking", the kind of the statement
	// whose lock the acknowledgement accepts, which the message names; it is
	// the zero Kind for every other acknowledgement.
	LockingStatement statement.Kind
}

// Error implements the error interface.
func (e *AcknowledgementMismatchError) Error() string {
	sentinel := e.Unwrap()
	if sentinel == nil {
		return fmt.Sprintf("the %s acknowledgement must name %q, got %q; nothing was executed",
			e.Acknowledgement, e.Table, e.Given)
	}
	if ackKind(e.Acknowledgement) == ackAcceptBlocking {
		return fmt.Sprintf("%s: %s locks %q, got %q; nothing was executed", sentinel, e.LockingStatement, e.Table, e.Given)
	}
	return fmt.Sprintf("%s %q, got %q; nothing was executed", sentinel, e.Table, e.Given)
}

// Unwrap returns the acknowledgement's own mismatch sentinel, or nil when
// Acknowledgement is not one [Acknowledgements] returns.
func (e *AcknowledgementMismatchError) Unwrap() error {
	return ackKind(e.Acknowledgement).mismatchErr()
}

// mismatchErr is the sentinel a wrong-table acknowledgement of this kind
// answers to errors.Is.
func (a ackKind) mismatchErr() error {
	switch a {
	case ackForce:
		return ErrForceMismatch
	case ackAcceptBlocking:
		return ErrAcceptBlockingMismatch
	case ackAllowDestructive:
		return ErrAllowDestructiveMismatch
	case ackRestart:
		return ErrRestartMismatch
	case ackBlockWrites:
		return ErrBlockWritesMismatch
	default:
		return nil
	}
}

// mismatch is the usage error for an acknowledgement of this kind that
// gave instead of naming table. The accept-blocking error names the locking
// statement, so it is built by acceptBlockingMismatch instead.
func (a ackKind) mismatch(table, given string) *AcknowledgementMismatchError {
	return &AcknowledgementMismatchError{Acknowledgement: string(a), Table: table, Given: given}
}

// acceptBlockingMismatch is the usage error for an accept-blocking
// acknowledgement that gave instead of naming table, the table that a
// statement of kind locks takes its lock on.
func acceptBlockingMismatch(table, given string, locks statement.Kind) *AcknowledgementMismatchError {
	return &AcknowledgementMismatchError{
		Acknowledgement:  string(ackAcceptBlocking),
		Table:            table,
		Given:            given,
		LockingStatement: locks,
	}
}
