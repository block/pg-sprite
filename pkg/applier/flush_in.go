package applier

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// FlushIn applies one drained batch inside a transaction the caller owns
// and leaves it open: it is the drain the cutover runs under its swap
// lock, where the writes must commit with the renames or not at all. The
// batch is applied as Flush applies it — completions first, then markers
// and images column-wise, with the whole-row fallback (CO-6) — after the
// same guards: the source and shadow are held in ACCESS SHARE, the lock
// session's backend is confirmed to hold the table from this connection,
// both relations are confirmed to be the ones the proofs describe, and,
// for a copy that evaluates conversion expressions, the catalog they
// resolve in is confirmed to be the one the build saw, before the writes
// and again after them (CO-9). The caller's transaction already carries
// its budgets and role, so none are set here; the flush pins
// extra_float_digits for its completions and sets the search_path every
// flush runs under — the catalog alone, or the build's path for a copy
// with conversions, so their expressions resolve as the gated statement's
// did (CO-9). It restores the digits before it returns and leaves the
// search_path on the flush's path for the caller to set back as its own
// statements need.
//
// The second resolution check is what refuses a write that resolved
// through a catalog change committed after the first; it sees that change
// only when the caller's transaction is READ COMMITTED, where each
// statement takes a new snapshot.
//
// A batch the shadow refuses even whole-row is ErrBatchDeferred, and the
// server has aborted tx: the caller rolls it back. Any other failure
// leaves tx aborted, or — for a resolution that drifted after the writes,
// ErrInvariantViolation — open with writes the caller must not commit;
// either way the caller rolls it back. An empty batch touches nothing, not
// even the session settings.
func (f *Flusher) FlushIn(ctx context.Context, tx pgx.Tx, batch Batch) (Result, error) {
	if len(batch.Entries) == 0 {
		return Result{}, nil
	}
	ctx, unbind := f.lock.Bind(ctx)
	defer unbind()
	result, err := f.flushIn(ctx, tx, batch)
	if err != nil {
		return Result{}, f.lostOr(err)
	}
	if lost := f.lockLost(); lost != nil {
		return Result{}, lost
	}
	return result, nil
}

func (f *Flusher) flushIn(ctx context.Context, tx pgx.Tx, batch Batch) (Result, error) {
	digits, err := pinFlushSettings(ctx, tx, f.searchPath)
	if err != nil {
		return Result{}, err
	}
	if err := f.holdRelations(ctx, tx); err != nil {
		return Result{}, err
	}
	if err := f.confirmLock(ctx, tx); err != nil {
		return Result{}, err
	}
	if err := f.confirmRelations(ctx, tx); err != nil {
		return Result{}, err
	}
	if err := f.confirmResolution(ctx, tx); err != nil {
		return Result{}, err
	}
	result, err := f.apply(ctx, tx, batch)
	if err != nil {
		return Result{}, err
	}
	// As in flush: a catalog change committed between the first check and
	// the writes is seen by this one, and the caller rolls the batch back
	// with its transaction (CO-9).
	if err := f.confirmResolution(ctx, tx); err != nil {
		return Result{}, err
	}
	if _, err := tx.Exec(ctx, "SET LOCAL extra_float_digits = "+strconv.Itoa(digits)); err != nil {
		return Result{}, fmt.Errorf("restore extra_float_digits after the flush: %w", err)
	}
	return result, nil
}

// pinFlushSettings sets the two session settings a flush depends on inside
// the caller's transaction — extra_float_digits and searchPath, the path
// statement the Flusher runs every flush under — and returns the
// extra_float_digits it found, so the caller's own value can be put back
// once the flush has read its completions. The setting is an integer on
// every supported server.
func pinFlushSettings(ctx context.Context, tx pgx.Tx, searchPath string) (int, error) {
	var text string
	if err := tx.QueryRow(ctx, `SELECT pg_catalog.current_setting('extra_float_digits')`).Scan(&text); err != nil {
		return 0, fmt.Errorf("read extra_float_digits before the flush: %w", err)
	}
	digits, err := strconv.Atoi(text)
	if err != nil {
		return 0, fmt.Errorf("read extra_float_digits before the flush: %q is not an integer: %w", text, err)
	}
	// INV: CO-9
	if _, err := tx.Exec(ctx, "SET LOCAL extra_float_digits = 3; "+searchPath); err != nil {
		return 0, fmt.Errorf("set flush settings: %w", err)
	}
	return digits, nil
}
