package migrate

import (
	"fmt"

	"github.com/block/pg-sprite/pkg/statement"
)

// AcknowledgementMismatchError is the usage error [Run] returns when a typed
// acknowledgement names a table other than the one it must name. Nothing has
// executed. errors.Is answers the acknowledgement's own sentinel —
// [ErrForceMismatch], [ErrAcceptBlockingMismatch],
// [ErrAllowDestructiveMismatch], [ErrRestartMismatch], or
// [ErrBlockWritesMismatch] — and no other, so an adapter that asks whether
// an error is any wrong-table acknowledgement uses errors.As once, and one
// that shows the operator what they gave and what the target is reads the
// fields rather than the message.
type AcknowledgementMismatchError struct {
	// Acknowledgement is the acknowledgement's name as [Acknowledgements]
	// spells it, such as "force".
	Acknowledgement string
	// Table is the schema-qualified table the acknowledgement must name:
	// the resolved target, or for "accept-blocking" the table the statement
	// locks.
	Table string
	// Given is the value the acknowledgement carried.
	Given string
	// locks is the kind of the statement whose lock an "accept-blocking"
	// acknowledgement accepts, which its message names.
	locks statement.Kind
}

// Error implements the error interface.
func (e *AcknowledgementMismatchError) Error() string {
	sentinel := ackKind(e.Acknowledgement).mismatchErr()
	if sentinel == nil {
		return fmt.Sprintf("the %s acknowledgement must name %q, got %q; nothing was executed",
			e.Acknowledgement, e.Table, e.Given)
	}
	if ackKind(e.Acknowledgement) == ackAcceptBlocking {
		return fmt.Sprintf("%s: %s locks %q, got %q; nothing was executed", sentinel, e.locks, e.Table, e.Given)
	}
	return fmt.Sprintf("%s %q, got %q; nothing was executed", sentinel, e.Table, e.Given)
}

// Is reports whether target is the acknowledgement's own mismatch sentinel.
func (e *AcknowledgementMismatchError) Is(target error) bool {
	sentinel := ackKind(e.Acknowledgement).mismatchErr()
	return sentinel != nil && target == sentinel
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
// gave instead of naming table.
func (a ackKind) mismatch(table, given string) *AcknowledgementMismatchError {
	return &AcknowledgementMismatchError{Acknowledgement: string(a), Table: table, Given: given}
}
