package applier

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// FlushIn applies one drained batch inside a transaction the caller owns
// and leaves it open: it is the drain the cutover runs under its swap
// lock, where the writes must commit with the renames or not at all. The
// batch is applied as Flush applies it — completions first, then markers
// and images column-wise, with the whole-row fallback (CO-6) — after the
// same guards: the source and shadow are held in ACCESS SHARE, the lock
// session's backend is confirmed to hold the table from this connection,
// and both relations are confirmed to be the ones the proofs describe.
// The caller's transaction already carries its budgets and role, so none
// are set here; the flush pins extra_float_digits for its completions and
// puts the catalog alone on the search_path (CO-9), restores the digits
// before it returns, and leaves the search_path on the catalog for the
// caller to set back as its own statements need.
//
// A batch the shadow refuses even whole-row is ErrBatchDeferred, and the
// server has aborted tx: the caller rolls it back. Any other failure
// leaves tx as the failed statement left it, aborted. An empty batch
// touches nothing, not even the session settings.
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
	digits, err := pinFlushSettings(ctx, tx)
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
	result, err := f.apply(ctx, tx, batch)
	if err != nil {
		return Result{}, err
	}
	if _, err := tx.Exec(ctx, "SET LOCAL extra_float_digits = "+strconv.Itoa(digits)); err != nil {
		return Result{}, fmt.Errorf("restore extra_float_digits after the flush: %w", err)
	}
	return result, nil
}

// pinFlushSettings sets the two session settings a flush depends on inside
// the caller's transaction and returns the extra_float_digits it found, so
// the caller's own value can be put back once the flush has read its
// completions. The setting is an integer on every supported server.
func pinFlushSettings(ctx context.Context, tx pgx.Tx) (int, error) {
	var text string
	if err := tx.QueryRow(ctx, `SELECT pg_catalog.current_setting('extra_float_digits')`).Scan(&text); err != nil {
		return 0, fmt.Errorf("read extra_float_digits before the flush: %w", err)
	}
	digits, err := strconv.Atoi(text)
	if err != nil {
		return 0, fmt.Errorf("read extra_float_digits before the flush: %q is not an integer: %w", text, err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL extra_float_digits = 3; "+dbconn.LocalSearchPath("pg_catalog")); err != nil {
		return 0, fmt.Errorf("set flush settings: %w", err)
	}
	return digits, nil
}
