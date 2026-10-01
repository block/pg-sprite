package checkpoint

import (
	"errors"
	"fmt"
)

// ErrNotFound reports that the store holds no row for the target. It is a
// positive answer from the database — the read completed and found nothing
// — and the only Load outcome from which a caller may start fresh (ST-2).
var ErrNotFound = errors.New("checkpoint: no row for target")

// ErrInvalidCheckpoint reports a Checkpoint the store refuses to persist:
// a missing identifier, an empty fingerprint, or an unknown phase.
var ErrInvalidCheckpoint = errors.New("checkpoint: invalid checkpoint")

// ErrInvalidOptions reports Options the store cannot run under.
var ErrInvalidOptions = errors.New("checkpoint: invalid options")

// ErrInvariantViolation reports a state the store's own invariants rule out:
// a row the database returned that no Save could have written.
var ErrInvariantViolation = errors.New("checkpoint: invariant violation")

// Mismatch names which identity field of a stored row disagrees with the
// run asking for it.
type Mismatch uint8

const (
	// MismatchFormat means the row was written in another row format: an
	// engine whose checkpoint columns mean something else.
	MismatchFormat Mismatch = iota + 1
	// MismatchSource means the row was written for a source table whose
	// introspected model differs from the current one.
	MismatchSource
	// MismatchTarget means the row was written for a different statement:
	// its after-schema model differs from the current one.
	MismatchTarget
)

// String returns the stable mismatch name.
func (m Mismatch) String() string {
	switch m {
	case MismatchFormat:
		return "format_version"
	case MismatchSource:
		return "source_fingerprint"
	case MismatchTarget:
		return "target_fingerprint"
	default:
		return fmt.Sprintf("Mismatch(%d)", m)
	}
}

// IncompatibleError reports a stored row the current run must not resume
// from or write over: it belongs to another row format or another statement
// (ST-2). Have is the stored value and Want the current run's.
type IncompatibleError struct {
	Schema   string
	Table    string
	Mismatch Mismatch
	Have     string
	Want     string
}

// Error names the target, the disagreeing field, and both values.
func (e *IncompatibleError) Error() string {
	return fmt.Sprintf("checkpoint for %s.%s is incompatible: stored %s %q, this run has %q",
		e.Schema, e.Table, e.Mismatch, e.Have, e.Want)
}
