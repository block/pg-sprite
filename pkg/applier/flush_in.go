package applier

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/internal/chunksql"
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
// are set here; the flush pins the output settings Flush pins,
// chunksql.Rendering, so a completion or conversion renders a value as the
// copy did, and puts the catalog alone on the search_path (CO-9). It
// restores the caller's extra_float_digits and bytea_output before it
// returns, and leaves the search_path on the catalog for the caller to set
// back as its own statements need.
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
	digits, byteaOutput, err := pinFlushSettings(ctx, tx)
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
	if _, err := tx.Exec(ctx, `SELECT pg_catalog.set_config('bytea_output', $1, true)`, byteaOutput); err != nil {
		return Result{}, fmt.Errorf("restore bytea_output after the flush: %w", err)
	}
	return result, nil
}

// pinFlushSettings sets the session settings a flush depends on inside the
// caller's transaction — the output settings of chunksql.Rendering and the
// search_path — and returns the extra_float_digits and bytea_output it
// found, so the caller's own values can be put back once the flush has
// written. extra_float_digits is an integer on every supported server.
func pinFlushSettings(ctx context.Context, tx pgx.Tx) (int, string, error) {
	var text, byteaOutput string
	if err := tx.QueryRow(ctx, `SELECT pg_catalog.current_setting('extra_float_digits'), pg_catalog.current_setting('bytea_output')`).Scan(&text, &byteaOutput); err != nil {
		return 0, "", fmt.Errorf("read extra_float_digits and bytea_output before the flush: %w", err)
	}
	digits, err := strconv.Atoi(text)
	if err != nil {
		return 0, "", fmt.Errorf("read extra_float_digits before the flush: %q is not an integer: %w", text, err)
	}
	if _, err := tx.Exec(ctx, chunksql.Rendering+"; "+dbconn.LocalSearchPath("pg_catalog")); err != nil {
		return 0, "", fmt.Errorf("set flush settings: %w", err)
	}
	return digits, byteaOutput, nil
}
