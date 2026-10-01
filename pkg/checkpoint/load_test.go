package checkpoint

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/decode"
)

// recordingSleep stands in for the wait between attempts and records every
// backoff it was asked for.
type recordingSleep struct{ waits []time.Duration }

func (s *recordingSleep) sleep(_ context.Context, d time.Duration) error {
	s.waits = append(s.waits, d)
	return nil
}

// A transient error is retried with a linearly growing backoff until the
// read succeeds; the attempt that succeeds is not followed by a wait.
func TestRetryTransientRetriesThroughTransientErrors(t *testing.T) {
	sleep := &recordingSleep{}
	calls := 0
	err := retryTransient(t.Context(), 5, 10*time.Millisecond, func(context.Context) error {
		calls++
		if calls < 3 {
			return &pgconn.PgError{Code: "57P01"}
		}
		return nil
	}, sleep.sleep)
	require.NoError(t, err)
	assert.Equal(t, 3, calls)
	assert.Equal(t, []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}, sleep.waits)
}

// No row is not a transient error: the read completed. It returns after one
// attempt, unwrapped, so Load can turn it into ErrNotFound.
func TestRetryTransientReturnsNoRowsAtOnce(t *testing.T) {
	sleep := &recordingSleep{}
	calls := 0
	err := retryTransient(t.Context(), 5, 10*time.Millisecond, func(context.Context) error {
		calls++
		return pgx.ErrNoRows
	}, sleep.sleep)
	assert.ErrorIs(t, err, pgx.ErrNoRows)
	assert.Equal(t, 1, calls)
	assert.Empty(t, sleep.waits)
}

// A permanent error — the table is missing — is not retried either.
func TestRetryTransientReturnsPermanentErrorAtOnce(t *testing.T) {
	sleep := &recordingSleep{}
	calls := 0
	err := retryTransient(t.Context(), 5, 10*time.Millisecond, func(context.Context) error {
		calls++
		return &pgconn.PgError{Code: "42P01"}
	}, sleep.sleep)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "42P01", pgErr.Code)
	assert.Equal(t, 1, calls)
	assert.Empty(t, sleep.waits)
}

// Exhausting the attempts returns the last transient error, still
// classifiable, after attempts-1 waits.
func TestRetryTransientExhaustsItsAttempts(t *testing.T) {
	sleep := &recordingSleep{}
	calls := 0
	err := retryTransient(t.Context(), 3, time.Millisecond, func(context.Context) error {
		calls++
		return &pgconn.PgError{Code: "08006"}
	}, sleep.sleep)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "08006", pgErr.Code)
	assert.Equal(t, 3, calls)
	assert.Equal(t, []time.Duration{time.Millisecond, 2 * time.Millisecond}, sleep.waits)
	assert.NotErrorIs(t, err, pgx.ErrNoRows)
}

// A context that ends during a wait stops the retries with the context's
// error, carrying the last read error for the operator.
func TestRetryTransientStopsWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	calls := 0
	err := retryTransient(ctx, 5, time.Millisecond, func(context.Context) error {
		calls++
		return &pgconn.PgError{Code: "57P01"}
	}, func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	})
	assert.ErrorIs(t, err, context.Canceled)
	var pgErr *pgconn.PgError
	assert.ErrorAs(t, err, &pgErr)
	assert.Equal(t, 1, calls)
}

// fakeRow answers one Scan from a fixed value list or with an error.
type fakeRow struct {
	values []any
	err    error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		return errors.New("fakeRow: column count mismatch")
	}
	for i, d := range dest {
		switch d := d.(type) {
		case *int32:
			*d = r.values[i].(int32)
		case *string:
			*d = r.values[i].(string)
		case *pgtype.Int8:
			*d = r.values[i].(pgtype.Int8)
		case *time.Time:
			*d = r.values[i].(time.Time)
		default:
			return errors.New("fakeRow: unsupported destination")
		}
	}
	return nil
}

// fakeQuerier serves a scripted sequence of rows, one per QueryRow call.
type fakeQuerier struct {
	rows  []fakeRow
	calls int
}

func (q *fakeQuerier) QueryRow(context.Context, string, ...any) pgx.Row {
	row := q.rows[q.calls]
	q.calls++
	return row
}

var (
	stampedAt = time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	wantFP    = Fingerprints{Source: "src-a", Target: "tgt-a"}
)

// storedRow is the row the fakes serve: a copy stopped at key 1000 with
// changes applied through 16/B374D848.
func storedRow() fakeRow {
	return fakeRow{values: []any{
		FormatVersion, "_pgsprite_orders_new", "pgsprite_0a1b2c3d", "pgsprite_0a1b2c3d",
		pgtype.Int8{Int64: 1000, Valid: true}, "16/B374D848", wantFP.Source, wantFP.Target, "copying", stampedAt,
	}}
}

func newTestStore(t *testing.T, sleep SleepFunc) *Store {
	t.Helper()
	return &Store{opts: Options{Clock: fixedClock{stampedAt}, LoadAttempts: 3, LoadBackoff: time.Millisecond, Sleep: sleep}}
}

// Two terminated-backend errors followed by a good read resume from the
// stored row; every column comes back typed.
func TestLoadRetriesATerminatedBackendAndDecodesTheRow(t *testing.T) {
	sleep := &recordingSleep{}
	q := &fakeQuerier{rows: []fakeRow{
		{err: &pgconn.PgError{Code: "57P01"}},
		{err: &pgconn.PgError{Code: "08006"}},
		storedRow(),
	}}
	cp, err := newTestStore(t, sleep.sleep).load(t.Context(), q, "app", "orders", wantFP)
	require.NoError(t, err)
	assert.Equal(t, 3, q.calls)
	assert.Len(t, sleep.waits, 2)
	assert.Equal(t, Checkpoint{
		Schema: "app", Table: "orders", ShadowTable: "_pgsprite_orders_new",
		SlotName: "pgsprite_0a1b2c3d", PublicationName: "pgsprite_0a1b2c3d",
		Watermark: copier.NewWatermark(1000), LastAppliedLSN: decode.LSN(0x00000016b374d848),
		SourceFingerprint: "src-a", TargetFingerprint: "tgt-a", Phase: PhaseCopying, UpdatedAt: stampedAt,
	}, cp)
}

// A blip that outlasts the attempts is an error that is neither ErrNotFound
// nor IncompatibleError: the caller must not start fresh on it (ST-2).
func TestLoadExhaustedRetriesIsNeitherNotFoundNorIncompatible(t *testing.T) {
	sleep := &recordingSleep{}
	q := &fakeQuerier{rows: []fakeRow{
		{err: &pgconn.PgError{Code: "57P01"}},
		{err: &pgconn.PgError{Code: "57P01"}},
		{err: &pgconn.PgError{Code: "57P01"}},
	}}
	_, err := newTestStore(t, sleep.sleep).load(t.Context(), q, "app", "orders", wantFP)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrNotFound)
	var incompatible *IncompatibleError
	assert.False(t, errors.As(err, &incompatible))
	var pgErr *pgconn.PgError
	assert.ErrorAs(t, err, &pgErr)
	assert.Equal(t, 3, q.calls)
}

// No row is ErrNotFound after a single read.
func TestLoadReportsNoRowAsNotFound(t *testing.T) {
	sleep := &recordingSleep{}
	q := &fakeQuerier{rows: []fakeRow{{err: pgx.ErrNoRows}}}
	_, err := newTestStore(t, sleep.sleep).load(t.Context(), q, "app", "orders", wantFP)
	assert.ErrorIs(t, err, ErrNotFound)
	assert.Equal(t, 1, q.calls)
	assert.Empty(t, sleep.waits)
}

// The format version is judged before the fingerprints: a row in another
// format is incompatible even when its fingerprint columns happen to match,
// because their meaning is not trusted. Among the fingerprints, the source
// is judged before the target.
func TestRowIncompatibilityOrdersFormatThenSourceThenTarget(t *testing.T) {
	base := row{schema: "app", table: "orders", formatVersion: FormatVersion, sourceFingerprint: "src-a", targetFingerprint: "tgt-a"}

	assert.Nil(t, base.incompatibility(wantFP))

	other := base
	other.formatVersion = FormatVersion + 1
	other.sourceFingerprint = "src-b"
	assert.Equal(t, &IncompatibleError{Schema: "app", Table: "orders", Mismatch: MismatchFormat, Have: "2", Want: "1"},
		other.incompatibility(wantFP))

	other = base
	other.sourceFingerprint = "src-b"
	other.targetFingerprint = "tgt-b"
	assert.Equal(t, &IncompatibleError{Schema: "app", Table: "orders", Mismatch: MismatchSource, Have: "src-b", Want: "src-a"},
		other.incompatibility(wantFP))

	other = base
	other.targetFingerprint = "tgt-b"
	assert.Equal(t, &IncompatibleError{Schema: "app", Table: "orders", Mismatch: MismatchTarget, Have: "tgt-b", Want: "tgt-a"},
		other.incompatibility(wantFP))
}

// Load judges compatibility on the row it read, so a different statement's
// row surfaces as a typed IncompatibleError, not as a Checkpoint.
func TestLoadReportsAnotherStatementsRowAsIncompatible(t *testing.T) {
	q := &fakeQuerier{rows: []fakeRow{storedRow()}}
	_, err := newTestStore(t, (&recordingSleep{}).sleep).load(t.Context(), q, "app", "orders", Fingerprints{Source: "src-a", Target: "tgt-other"})
	var incompatible *IncompatibleError
	require.ErrorAs(t, err, &incompatible)
	assert.Equal(t, MismatchTarget, incompatible.Mismatch)
	assert.Equal(t, "tgt-a", incompatible.Have)
	assert.Equal(t, "tgt-other", incompatible.Want)
	assert.NotErrorIs(t, err, ErrNotFound)
}

// A NULL watermark decodes to the zero Watermark: nothing has landed.
func TestRowCheckpointDecodesNullWatermarkAsNothingLanded(t *testing.T) {
	r := row{formatVersion: FormatVersion, lastAppliedLSN: "0/0", phase: "copying"}
	cp, err := r.checkpoint()
	require.NoError(t, err)
	assert.False(t, cp.Watermark.Valid())
	assert.Equal(t, decode.LSN(0), cp.LastAppliedLSN)
}

// A value Save cannot have written is an invariant violation, not a resume
// point: a phase name String never produces, or an LSN that does not parse.
func TestRowCheckpointRefusesValuesSaveCannotHaveWritten(t *testing.T) {
	bad := row{formatVersion: FormatVersion, lastAppliedLSN: "0/0", phase: "paused"}
	_, err := bad.checkpoint()
	assert.ErrorIs(t, err, ErrInvariantViolation)

	bad = row{formatVersion: FormatVersion, lastAppliedLSN: "not-an-lsn", phase: "copying"}
	_, err = bad.checkpoint()
	assert.ErrorIs(t, err, ErrInvariantViolation)
}

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }
