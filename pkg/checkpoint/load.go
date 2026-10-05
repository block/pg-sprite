package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
)

// Load reads the target's checkpoint for a run identified by want and tells
// its four outcomes apart (ST-2): a row carrying want's format and
// fingerprints is returned; no row is ErrNotFound, the one outcome a caller
// may start fresh from; a row carrying another format version or another
// statement's fingerprints is an IncompatibleError and must not be resumed
// from or written over; a read that did not complete is retried through
// transient errors under the store's bounded attempts and, once they are
// spent, returned as an error that is none of the above — a blip never
// reads as "no checkpoint". A database where Ensure has never run has no
// table to read, which is ErrTableMissing: also none of the above, so a
// status reader can report "no checkpoint table" without resume mistaking
// it for ErrNotFound.
func (s *Store) Load(ctx context.Context, schema, table string, want Fingerprints) (Checkpoint, error) {
	return s.load(ctx, s.pool, schema, table, want)
}

func (s *Store) load(ctx context.Context, q dbconn.RowQuerier, schema, table string, want Fingerprints) (Checkpoint, error) {
	// INV: ST-2
	var stored row
	read := func(ctx context.Context) error {
		var err error
		stored, err = readRow(ctx, q, schema, table)
		return err
	}
	if err := retryTransient(ctx, s.opts.LoadAttempts, s.opts.LoadBackoff, read, s.opts.Sleep); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Checkpoint{}, fmt.Errorf("%w: %s.%s", ErrNotFound, schema, table)
		}
		return Checkpoint{}, fmt.Errorf("load checkpoint for %s.%s: %w", schema, table, missingTable(err))
	}
	if incompatible := stored.incompatibility(want); incompatible != nil {
		return Checkpoint{}, incompatible
	}
	cp, err := stored.checkpoint()
	if err != nil {
		return Checkpoint{}, fmt.Errorf("load checkpoint for %s.%s: %w", schema, table, err)
	}
	return cp, nil
}

// SQLSTATE codes a backend reports when the server ends its session from
// outside the session — pg_terminate_backend, a shutdown, or a restart still
// in recovery — which is how a failover looks from the client. A write
// interrupted this way has an ambiguous outcome and must not be retried,
// which is why dbconn.Retryable does not list them; Load is a read, so
// repeating it is safe and these count as transient for it alone.
const (
	codeAdminShutdown    = "57P01"
	codeCrashShutdown    = "57P02"
	codeCannotConnectNow = "57P03"
)

// readRetryable reports whether a failed read of the checkpoint row may be
// repeated: anything the engine treats as transient, plus a session the
// server ended from outside it.
func readRetryable(err error) bool {
	if dbconn.Retryable(err) {
		return true
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case codeAdminShutdown, codeCrashShutdown, codeCannotConnectNow:
		return true
	}
	return false
}

// retryTransient runs attempt up to attempts times, waiting between tries
// with a linearly growing backoff, and retries only errors readRetryable
// classifies as transient. Any other error, including no row, returns at
// once; the context's end wins over a pending wait.
func retryTransient(ctx context.Context, attempts int, backoff time.Duration, attempt func(context.Context) error, sleep SleepFunc) error {
	var last error
	for i := 1; i <= attempts; i++ {
		last = attempt(ctx)
		if last == nil {
			return nil
		}
		if !readRetryable(last) {
			return last
		}
		if i == attempts {
			break
		}
		if err := sleep(ctx, backoff*time.Duration(i)); err != nil {
			return fmt.Errorf("%w (last read error: %w)", err, last)
		}
	}
	return fmt.Errorf("retries exhausted after %d attempts: %w", attempts, last)
}

// row is one checkpoint row as the database returns it, before it is
// checked against the run and decoded into a Checkpoint.
type row struct {
	schema, table     string
	formatVersion     int32
	shadowTable       string
	slotName          string
	publicationName   string
	watermark         pgtype.Int8
	lastAppliedLSN    string
	sourceFingerprint string
	targetFingerprint string
	phase             string
	updatedAt         time.Time
}

// loadSQL reads the target's row. The LSN comes back in its text form; pgx
// has no codec for pg_lsn and decode.ParseLSN is the one reader of it.
const loadSQL = `SELECT format_version, shadow_table, slot_name, publication_name,
	watermark, last_applied_lsn::text, source_fingerprint, target_fingerprint, phase, updated_at
FROM ` + tableIdent + ` WHERE schema_name = $1 AND table_name = $2`

// readRow performs one read of the target's row and returns pgx.ErrNoRows
// unwrapped when there is none, so callers can tell absence from failure.
func readRow(ctx context.Context, q dbconn.RowQuerier, schema, table string) (row, error) {
	r := row{schema: schema, table: table}
	err := q.QueryRow(ctx, loadSQL, schema, table).Scan(
		&r.formatVersion, &r.shadowTable, &r.slotName, &r.publicationName,
		&r.watermark, &r.lastAppliedLSN, &r.sourceFingerprint, &r.targetFingerprint, &r.phase, &r.updatedAt)
	if err != nil {
		return row{}, err
	}
	return r, nil
}

// identity is the stored row's guarded fields.
func (r row) identity() Identity {
	return Identity{
		FormatVersion: r.formatVersion,
		Fingerprints:  Fingerprints{Source: r.sourceFingerprint, Target: r.targetFingerprint},
	}
}

// incompatibility reports the first identity field on which the stored row
// disagrees with the run, format version first: a row in another format
// cannot be trusted to carry fingerprints that mean the same thing. It is
// nil for a row the run may resume from.
func (r row) incompatibility(want Fingerprints) *IncompatibleError {
	return r.incompatibleWith(Identity{FormatVersion: FormatVersion, Fingerprints: want})
}

// incompatibleWith reports the first field on which the stored row's
// identity differs from want, in the same order; nil when they agree.
func (r row) incompatibleWith(want Identity) *IncompatibleError {
	// INV: ST-2
	mismatch := &IncompatibleError{Schema: r.schema, Table: r.table, Stored: r.identity()}
	switch {
	case r.formatVersion != want.FormatVersion:
		mismatch.Mismatch, mismatch.Have, mismatch.Want = MismatchFormat, fmt.Sprint(r.formatVersion), fmt.Sprint(want.FormatVersion)
	case r.sourceFingerprint != want.Fingerprints.Source:
		mismatch.Mismatch, mismatch.Have, mismatch.Want = MismatchSource, r.sourceFingerprint, want.Fingerprints.Source
	case r.targetFingerprint != want.Fingerprints.Target:
		mismatch.Mismatch, mismatch.Have, mismatch.Want = MismatchTarget, r.targetFingerprint, want.Fingerprints.Target
	default:
		return nil
	}
	return mismatch
}

// checkpoint decodes a compatible row. A value Save could not have written
// — an LSN that does not parse, a phase name String never produces — is an
// invariant violation, not a resume point.
func (r row) checkpoint() (Checkpoint, error) {
	lsn, err := decode.ParseLSN(r.lastAppliedLSN)
	if err != nil {
		return Checkpoint{}, fmt.Errorf("%w: %w", ErrInvariantViolation, err)
	}
	phase, err := parsePhase(r.phase)
	if err != nil {
		return Checkpoint{}, fmt.Errorf("%w: %w", ErrInvariantViolation, err)
	}
	var watermark copier.Watermark
	if r.watermark.Valid {
		watermark = copier.NewWatermark(r.watermark.Int64)
	}
	return Checkpoint{
		Schema:            r.schema,
		Table:             r.table,
		ShadowTable:       r.shadowTable,
		SlotName:          r.slotName,
		PublicationName:   r.publicationName,
		Watermark:         watermark,
		LastAppliedLSN:    lsn,
		SourceFingerprint: r.sourceFingerprint,
		TargetFingerprint: r.targetFingerprint,
		Phase:             phase,
		UpdatedAt:         r.updatedAt,
	}, nil
}
