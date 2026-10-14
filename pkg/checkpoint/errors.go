package checkpoint

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// ErrNotFound reports that the store holds no row for the target. It is a
// positive answer from the database — the read completed and found nothing
// — and the only Load outcome from which a caller may start fresh (ST-2).
var ErrNotFound = errors.New("checkpoint: no row for target")

// ErrTableMissing reports that the checkpoint table does not exist in the
// database: Ensure has never run there. It is distinct from ErrNotFound —
// a database with no table has no checkpoints to read, but that is not a
// positive "no row for this target" and resume must not start fresh from it
// — and it still carries the server's error for callers that want it.
var ErrTableMissing = errors.New("checkpoint: checkpoint table does not exist; Ensure has not run")

// ErrForeignObject reports that the engine schema or the checkpoint table
// exists but is not the engine's: another role owns it, or the name is
// taken by a relation that is not a plain table. Ensure refuses rather
// than adopt it, because whatever hangs off a foreign object — a trigger,
// a view — would run with the engine's privileges on every Save.
var ErrForeignObject = errors.New("checkpoint: engine object exists but is not the engine's")

// ErrTableNewer reports that the checkpoint table has columns this engine
// does not know. Ensure refuses rather than alter it, since this engine
// cannot preserve the meaning of columns it has never heard of, and leaves
// the table as it found it. The error a caller receives is a
// *TableNewerError naming the columns.
var ErrTableNewer = errors.New("checkpoint: checkpoint table has columns this engine does not know")

// ErrTableBehind reports that the checkpoint table lacks a column this
// engine reads or writes: an older engine created it and Ensure has not
// run since this engine was installed. The remedy is to run Ensure, which
// adds the column, and read again; it still carries the server's error
// for callers that want it.
var ErrTableBehind = errors.New("checkpoint: checkpoint table lacks a column this engine uses; Ensure has not run since the engine was upgraded")

// TableNewerError is the ErrTableNewer a caller receives: Unknown names the
// columns this engine does not know, in the table's column order. A column
// a newer engine added and a column an operator added by hand look the
// same from here, and both are refused the same way, so the message names
// both causes and what to do about each.
type TableNewerError struct {
	Unknown []string
}

// Error names the columns and the remedy for each cause.
func (e *TableNewerError) Error() string {
	return fmt.Sprintf("checkpoint: checkpoint table %s has columns this engine does not know: %s; run a pg-sprite release that knows them (upgrade, or roll forward after a rollback), or, if a column was added by hand and no schema change is in flight, drop it",
		tableIdent, strings.Join(e.Unknown, ", "))
}

// Is matches ErrTableNewer, so errors.Is(err, ErrTableNewer) holds for a
// *TableNewerError.
func (e *TableNewerError) Is(target error) bool { return target == ErrTableNewer }

// ErrInvalidCheckpoint reports a Checkpoint the store refuses to persist:
// a missing identifier, an empty fingerprint, an unknown phase, or a shadow
// proof that is not a JSON object the jsonb column can hold.
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
// (ST-2). Mismatch names the first disagreeing field, Have is its stored
// value and Want the current run's; Stored is the whole identity of the row
// as it was read, which a caller hands back to Delete so that a fresh start
// removes exactly the row it was shown and no later one.
type IncompatibleError struct {
	Schema   string
	Table    string
	Mismatch Mismatch
	Have     string
	Want     string
	Stored   Identity
}

// Error names the target, the disagreeing field, and both values.
func (e *IncompatibleError) Error() string {
	return fmt.Sprintf("checkpoint for %s.%s is incompatible: stored %s %q, this run has %q",
		e.Schema, e.Table, e.Mismatch, e.Have, e.Want)
}

// SQLSTATEs the server reports against the checkpoint table: undefined_table
// when it does not exist, undefined_column when it lacks a column the
// statement names. The store's statements name only the engine's own
// columns, so the second can only mean a table an older engine created.
const (
	undefinedTable  = "42P01"
	undefinedColumn = "42703"
)

// tableUnusable turns the server's undefined_table into ErrTableMissing and
// its undefined_column into ErrTableBehind, keeping the server's error
// reachable through errors.As; any other error is returned as it came.
func tableUnusable(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case undefinedTable:
		return fmt.Errorf("%w: %w", ErrTableMissing, err)
	case undefinedColumn:
		// INV: ST-10
		return fmt.Errorf("%w: %w", ErrTableBehind, err)
	}
	return err
}
