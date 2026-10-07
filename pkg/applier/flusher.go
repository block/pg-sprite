package applier

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
)

var (
	// ErrInvalidOptions reports flusher options that cannot bound a flush: a
	// timeout below PostgreSQL's one-millisecond resolution, which the server
	// would read as no timeout at all.
	ErrInvalidOptions = errors.New("invalid flusher options")
	// ErrBatchDeferred reports a batch the flush could not apply because a
	// unique index refused it even after the delete-all-then-insert-all
	// fallback (CO-6): a surviving image collides with a shadow row the batch
	// does not name — a stale row whose own deletion is buffered behind an
	// in-flight chunk. The transaction was rolled back and the shadow is as it
	// was; the stream owner puts the batch back with Buffer.Requeue and drains
	// again once the copier has moved.
	ErrBatchDeferred = errors.New("batch deferred: a unique index refused it")
)

// sqlstateUniqueViolation is the SQLSTATE a unique index raises when an
// insert or update would duplicate one of its keys.
const sqlstateUniqueViolation = "23505"

// Options bounds a flush. Zero values take the dbconn session timeout
// defaults.
type Options struct {
	// LockTimeout bounds every lock wait inside the flush transaction.
	LockTimeout time.Duration
	// StatementTimeout bounds every statement inside the flush transaction.
	StatementTimeout time.Duration
}

func (o Options) withDefaults() Options {
	if o.LockTimeout == 0 {
		o.LockTimeout = dbconn.DefaultLockTimeout
	}
	if o.StatementTimeout == 0 {
		o.StatementTimeout = dbconn.DefaultStatementTimeout
	}
	return o
}

// validate runs after withDefaults, so every field is set.
func (o Options) validate() error {
	for _, timeout := range []struct {
		name  string
		value time.Duration
	}{{"lock timeout", o.LockTimeout}, {"statement timeout", o.StatementTimeout}} {
		if timeout.value < time.Millisecond {
			// INV: LK-2
			return fmt.Errorf("%w: %s %s is below PostgreSQL's one-millisecond resolution; use zero for the default", ErrInvalidOptions, timeout.name, timeout.value)
		}
	}
	return nil
}

// Result is what one Flush wrote.
type Result struct {
	// Images is the number of row images written, by upsert or, under
	// Fallback, by whole-row insert.
	Images int
	// Deletes is the number of delete markers applied. Under Fallback every
	// key in the batch is deleted before the inserts; those deletes are not
	// counted here, only the markers.
	Deletes int
	// CompletedFromShadow is the number of images whose unchanged-TOAST
	// markers the flush filled in from a shadow row (D13).
	CompletedFromShadow int
	// CompletedFromSource is the number of moved images whose markers the
	// flush filled in from the source row under Key, because the old key's
	// shadow row was absent or another row's (D13).
	CompletedFromSource int
	// Skipped is the number of moved images the flush did not write because
	// neither the old key's shadow row nor the source row under Key could
	// complete them: the source has since deleted or moved the key again,
	// and that later event removes the key (D13).
	Skipped int
	// Fallback reports that a unique index refused the column-wise upserts
	// and the batch was applied as delete-all-then-insert-all (CO-6).
	Fallback bool
}

// Flusher writes drained batches into the shadow. Each Flush is one bounded
// read-write transaction under the table's lock session, guarded as the
// copier's chunk inserts are, that applies every entry of the batch once:
// a delete marker as a DELETE, an image as an upsert of its present columns
// (CO-5, CO-8), with the batch-wide fallback D13 decides when a unique
// index refuses the upserts (CO-6). A Flusher keeps no state between
// flushes; the stream owner calls Flush from one goroutine, after Drain and
// before the next Add.
type Flusher struct {
	target preflight.CopySwapTarget
	shadow copier.Shadow
	lock   *dbconn.TableLockSession
	opts   Options
	// columns are the shadow's copy columns other than the primary key, in
	// source order: the columns an image may write. A decoded column the
	// shadow does not hold is ignored; the key is taken from Entry.Key.
	columns []string
}

// NewFlusher prepares flushes into shadow for the proven target. It refuses
// a proof, shadow, or lock session that does not describe this table, and
// options that cannot bound a flush. The shadow is checked as the copier
// checks it: the flusher writes into the relation the copier writes into
// and must refuse everything the copier would have.
func NewFlusher(target preflight.CopySwapTarget, shadow copier.Shadow, lock *dbconn.TableLockSession, opts Options) (*Flusher, error) {
	// INV: ST-6
	if target.Table() == "" {
		return nil, fmt.Errorf("%w (ST-6): copy-and-swap target proof is empty", ErrInvariantViolation)
	}
	if err := checkShadow(target, shadow); err != nil {
		return nil, err
	}
	if err := requireTableLock(lock, target); err != nil {
		return nil, err
	}
	opts = opts.withDefaults()
	if err := opts.validate(); err != nil {
		return nil, err
	}
	var columns []string
	for _, c := range shadow.CopyColumns() {
		if c != target.PKColumn() {
			columns = append(columns, c)
		}
	}
	return &Flusher{target: target, shadow: shadow, lock: lock, opts: opts, columns: columns}, nil
}

// Flush applies one drained batch to the shadow in one transaction, or
// writes nothing. It completes every image Batch.CompleteFirst names before
// it writes anything (D13), then applies the entries in key order: a delete
// marker deletes the key's row, an image without markers upserts every
// column, an image with markers updates its present columns and must find
// the key's row (CO-8). When a unique index refuses an upsert the flush
// rolls back to its savepoint, completes every surviving image that still
// carries a marker from the shadow row under its key, deletes every key in
// the batch, and inserts every surviving image whole (CO-6); a unique index
// that refuses that too means a collision with a row the batch does not
// name, and the flush returns ErrBatchDeferred with the shadow unchanged.
// An image the source could not have produced given the shadow — a marker
// for a row the shadow does not hold — is ErrInvariantViolation, and the
// flush commits nothing. The transaction runs under the lock session's Bind
// context, so losing the table lock cancels the flush and it reports the
// loss. An empty batch is a no-op.
func (f *Flusher) Flush(ctx context.Context, pool *pgxpool.Pool, batch Batch) (Result, error) {
	if len(batch.Entries) == 0 {
		return Result{}, nil
	}
	ctx, unbind := f.lock.Bind(ctx)
	defer unbind()
	result, err := f.flush(ctx, pool, batch)
	if err != nil {
		return Result{}, f.lostOr(err)
	}
	if lost := f.lockLost(); lost != nil {
		return Result{}, lost
	}
	return result, nil
}

func (f *Flusher) flush(ctx context.Context, pool *pgxpool.Pool, batch Batch) (Result, error) {
	tx, err := f.begin(ctx, pool)
	if err != nil {
		return Result{}, err
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed; on a failure path the server aborts
		// the transaction with its session either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	var result Result
	entries, err := f.completeMoved(ctx, tx, batch, &result)
	if err != nil {
		return Result{}, err
	}
	// INV: CO-5, CO-6, CO-8
	if err := f.applyColumnWise(ctx, tx, entries, &result); err != nil {
		if !uniqueViolation(err) {
			return Result{}, err
		}
		result = Result{
			CompletedFromShadow: result.CompletedFromShadow,
			CompletedFromSource: result.CompletedFromSource,
			Skipped:             result.Skipped,
			Fallback:            true,
		}
		if err := f.applyWholeRows(ctx, tx, entries, &result); err != nil {
			if uniqueViolation(err) {
				return Result{}, fmt.Errorf("%w: %s.%s: %w", ErrBatchDeferred, f.shadow.Schema(), f.shadow.ShadowTable(), err)
			}
			return Result{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("commit flush into %s.%s: %w", f.shadow.Schema(), f.shadow.ShadowTable(), err)
	}
	return result, nil
}

// completeMoved fills in the markers of every image Batch.CompleteFirst
// names and returns the entries to write: the batch's, less any image the
// source has since removed. An image whose OldKey was not reused reads the
// shadow row under OldKey, locked so the delete marker at that key, applied
// later in this transaction, cannot race it; when that row is absent, or
// the image is flagged OldKeyReused, it reads the source row under Key
// instead; when the source row is absent too the image is skipped (D13).
func (f *Flusher) completeMoved(ctx context.Context, tx pgx.Tx, batch Batch, result *Result) ([]Entry, error) {
	skip := make(map[int64]bool)
	for _, e := range batch.CompleteFirst() {
		if !e.OldKeyReused {
			found, err := f.completeFrom(ctx, tx, f.shadowRowSQL(e), *e.OldKey, e)
			if err != nil {
				return nil, err
			}
			if found {
				result.CompletedFromShadow++
				continue
			}
		}
		found, err := f.completeFrom(ctx, tx, f.sourceRowSQL(e), e.Key, e)
		if err != nil {
			return nil, err
		}
		if found {
			result.CompletedFromSource++
			continue
		}
		skip[e.Key] = true
		result.Skipped++
	}
	if len(skip) == 0 {
		return batch.Entries, nil
	}
	entries := make([]Entry, 0, len(batch.Entries)-len(skip))
	for _, e := range batch.Entries {
		if !skip[e.Key] {
			entries = append(entries, e)
		}
	}
	return entries, nil
}

// applyColumnWise writes the entries in a savepoint, each by its kind, so a
// unique violation rolls the writes back and leaves the completions in
// place. A savepoint inside a pgx transaction is what Begin on it makes.
func (f *Flusher) applyColumnWise(ctx context.Context, tx pgx.Tx, entries []Entry, result *Result) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("save point before the column-wise flush: %w", err)
	}
	for i := range entries {
		e := &entries[i]
		switch e.Kind {
		case DeleteMarker:
			err = f.deleteRow(ctx, sp, e.Key)
			result.Deletes++
		case Image:
			err = f.writeImage(ctx, sp, e)
			result.Images++
		default:
			err = fmt.Errorf("%w (CO-5): flush key %d: unknown entry kind %s", ErrInvariantViolation, e.Key, e.Kind)
		}
		if err != nil {
			if rollbackErr := sp.Rollback(ctx); rollbackErr != nil {
				return errors.Join(err, fmt.Errorf("roll back to the save point: %w", rollbackErr))
			}
			return err
		}
	}
	if err := sp.Commit(ctx); err != nil {
		return fmt.Errorf("release the save point after the column-wise flush: %w", err)
	}
	return nil
}

// writeImage upserts an image that is whole and updates one that still
// carries a marker: the marker stands for the value in the shadow row under
// the image's own key — a moved image was completed before this — so that
// row must exist, and an update that finds no row is the protocol error D13
// names, not a row to invent (CO-8).
func (f *Flusher) writeImage(ctx context.Context, tx pgx.Tx, e *Entry) error {
	present, markers := f.split(e)
	if len(markers) == 0 {
		return f.upsertRow(ctx, tx, e.Key, present)
	}
	found, err := f.updateRow(ctx, tx, e.Key, present)
	if err != nil {
		return err
	}
	if !found {
		// INV: CO-8
		return fmt.Errorf("%w (CO-8): flush key %d: image omits %v and the shadow holds no row to keep them from", ErrInvariantViolation, e.Key, markers)
	}
	return nil
}

// applyWholeRows is the CO-6 fallback: complete every surviving image that
// still carries a marker from the shadow row under its own key — present by
// CO-8 and the drain's deferral rule, so an absent row is a protocol error —
// then delete every key in the batch and insert every surviving image whole,
// so no row the batch names is left to collide with the inserts (D13). The
// deletes come after every completion read, and the reads lock their rows.
func (f *Flusher) applyWholeRows(ctx context.Context, tx pgx.Tx, entries []Entry, result *Result) error {
	// INV: CO-6, CO-8
	keys := make([]int64, 0, len(entries))
	for i := range entries {
		e := &entries[i]
		keys = append(keys, e.Key)
		if e.Kind != Image || !e.HasMarker() {
			continue
		}
		found, err := f.completeFrom(ctx, tx, f.shadowRowSQL(e), e.Key, e)
		if err != nil {
			return err
		}
		if !found {
			_, markers := f.split(e)
			return fmt.Errorf("%w (CO-8): flush key %d: image omits %v and the shadow holds no row to complete them from", ErrInvariantViolation, e.Key, markers)
		}
		result.CompletedFromShadow++
	}
	if err := f.deleteRows(ctx, tx, keys); err != nil {
		return err
	}
	for i := range entries {
		e := &entries[i]
		switch e.Kind {
		case DeleteMarker:
			result.Deletes++
		case Image:
			present, markers := f.split(e)
			if len(markers) != 0 {
				return fmt.Errorf("%w (CO-8): flush key %d: image omits %v after completion", ErrInvariantViolation, e.Key, markers)
			}
			if err := f.insertRow(ctx, tx, e.Key, present); err != nil {
				return err
			}
			result.Images++
		}
	}
	return nil
}

// uniqueViolation reports whether err is the server refusing a write on a
// unique index.
func uniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == sqlstateUniqueViolation
}
