package schemachange

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/preflight"
)

// MeasureResumeHeld reads from the server what a stopped run on the shape's
// table already holds on the volume, for the environment check to discount
// from its disk requirement (preflight.CopySwapEnvironment.Held): the
// shadow's total size, and, for a run that decodes WAL, the WAL the
// route's slot retains. The checkpoint row only says whether there is a
// run to resume; the shadow and the slot are found by the names the route
// derives for the table, and their sizes are read from the catalog, never
// from the row.
//
// A cutover row whose swap the catalog shows committed is reported as
// such (CopySwapHeld.SwapCommitted), and nothing else is measured: the
// shadow already bears the table's name, and the run that resumes it only
// finishes the swap. The catalog read is the one InspectSwapped makes, so
// the preflight and the run answer the row alike; a cutover row whose
// swap did not commit, or whose outcome that read refuses to call, is
// measured as any other resume.
//
// Every reading that does not prove space held counts nothing, so the
// check falls back to a fresh run's requirement: no row, no checkpoint
// table, a row this engine cannot read (the run itself refuses it), a
// terminal row (a done row is replaced by a fresh run), no relation under
// the shadow's name or one dropped while it is measured, a row that names
// no slot, and a slot that is missing, another database's, lost, or whose
// retained WAL the server cannot measure. A read that fails is returned,
// and the preflight does not finish: nothing has been written then. The
// reads take no lock on the table, so a run holding the table's lock may
// still grow what is measured; that run keeps this one from starting, and
// growth only adds held space. The shadow's size read does lock the
// shadow, so it waits out a concurrent DDL on it.
func MeasureResumeHeld(ctx context.Context, pool *pgxpool.Pool, store *checkpoint.Store, shape preflight.CopySwapShape, decodesWAL bool) (preflight.CopySwapHeld, error) {
	if shape.Table() == "" {
		return preflight.CopySwapHeld{}, fmt.Errorf("%w: zero copy-and-swap shape", preflight.ErrCopySwapProofMismatch)
	}
	cp, resumable, err := resumableRow(ctx, store, shape.Schema(), shape.Table())
	if err != nil {
		return preflight.CopySwapHeld{}, err
	}
	if !resumable {
		return preflight.CopySwapHeld{}, nil
	}
	committed, err := swapCommitted(ctx, pool, cp)
	if err != nil {
		return preflight.CopySwapHeld{}, err
	}
	if committed {
		return preflight.CopySwapHeld{SwapCommitted: true}, nil
	}
	var held preflight.CopySwapHeld
	if held.ShadowBytes, err = shadowBytes(ctx, pool, shape); err != nil {
		return preflight.CopySwapHeld{}, err
	}
	if !decodesWAL || cp.SlotName == "" {
		return held, nil
	}
	if held.RetainedWALBytes, err = slotRetainedBytes(ctx, pool, shape.DecodingName()); err != nil {
		return preflight.CopySwapHeld{}, err
	}
	return held, nil
}

// resumableRow reads the table's checkpoint row and reports whether it
// records a run the next run resumes: a row in a phase this engine knows
// that is not terminal. The row-level outcomes Inspect reports — no row,
// no table, a table an older engine shaped, a row in another format or an
// unknown phase — are not resumes here; the run decides those itself under
// its lock. Any other error is returned.
func resumableRow(ctx context.Context, store *checkpoint.Store, schema, table string) (checkpoint.Checkpoint, bool, error) {
	cp, err := store.Inspect(ctx, schema, table)
	switch {
	case err == nil:
		return cp, !cp.Phase.Terminal(), nil
	case errors.Is(err, checkpoint.ErrNotFound),
		errors.Is(err, checkpoint.ErrTableMissing),
		errors.Is(err, checkpoint.ErrTableBehind):
		return checkpoint.Checkpoint{}, false, nil
	default:
		if _, ok := errors.AsType[*checkpoint.IncompatibleError](err); ok {
			return checkpoint.Checkpoint{}, false, nil
		}
		return checkpoint.Checkpoint{}, false, fmt.Errorf("read checkpoint row for %s.%s: %w", schema, table, err)
	}
}

// swapCommitted reports whether cp is a cutover row whose swap the catalog
// shows committed, retained source dropped or not, by the read
// InspectSwapped makes, in one bounded read-only transaction on the pool.
// It takes no table lock: the run that follows reads the same catalog
// again under its lock before it acts. Every answer that does not prove
// the commit is false, so the row is measured as a resume that still
// copies: a row in another phase; a row whose proof does not decode,
// which the run refuses; the source still live (ErrNotSwapped); and a
// catalog the read refuses to call — a swap still deciding, or relations
// the proof does not describe. A read that fails is returned.
func swapCommitted(ctx context.Context, pool *pgxpool.Pool, cp checkpoint.Checkpoint) (bool, error) {
	if cp.Phase != checkpoint.PhaseCutover {
		return false, nil
	}
	proof, err := rowProof(cp)
	if err != nil {
		_, err := swapOutcome(err)
		return false, err
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return false, fmt.Errorf("begin swap outcome read for %s.%s: %w", cp.Schema, cp.Table, err)
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed; on a failure path the server aborts
		// the transaction with its session either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	if err := setBudgets(ctx, tx, Options{}); err != nil {
		return false, err
	}
	if err := setSearchPath(ctx, tx, proof.Schema); err != nil {
		return false, err
	}
	// INV: LK-4 — the commit is read from the catalog, never assumed from
	// the row's phase.
	_, readErr := readSwappedTable(ctx, tx, proof)
	committed, err := swapOutcome(readErr)
	if err != nil {
		return false, fmt.Errorf("read swap outcome for %s.%s: %w", cp.Schema, cp.Table, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit swap outcome read for %s.%s: %w", cp.Schema, cp.Table, err)
	}
	return committed, nil
}

// swapOutcome reads what readSwappedTable, or rowProof before it,
// returned as whether the swap committed: a minted proof and a retained
// source already dropped prove it; the source still live and a refusal —
// which leaves the outcome for the run to answer under its lock — do not.
// Any other error is returned: the read did not finish.
func swapOutcome(err error) (bool, error) {
	switch {
	case err == nil, errors.Is(err, ErrOldTableNotFound):
		return true, nil
	case errors.Is(err, ErrNotSwapped):
		return false, nil
	}
	if _, ok := errors.AsType[*RefusalError](err); ok {
		return false, nil
	}
	return false, err
}

// shadowBytesSQL reads the total size of the ordinary table that wears the
// shadow's name in the source schema, and nothing when the pool is not on
// the shape's database or no such table exists. Every name is resolved
// through pg_catalog, whatever the pool's search_path (CO-9).
const shadowBytesSQL = `
	SELECT pg_catalog.pg_total_relation_size(c.oid)
	FROM pg_catalog.pg_class c
	JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
	WHERE n.nspname OPERATOR(pg_catalog.=) $1
	  AND c.relname OPERATOR(pg_catalog.=) $2
	  AND c.relkind OPERATOR(pg_catalog.=) 'r'
	  AND pg_catalog.current_database() OPERATOR(pg_catalog.=) $3`

// shadowBytes is the server's size of the shape's shadow, zero when no
// table wears its name. The size read takes ACCESS SHARE on the shadow, so
// it waits out a transaction that holds a stronger lock on it, and a
// shadow that transaction dropped reads NULL: it is gone, and counts
// nothing, as a shadow that was never there does.
func shadowBytes(ctx context.Context, pool *pgxpool.Pool, shape preflight.CopySwapShape) (int64, error) {
	shadow := ShadowName(shape.Schema(), shape.Table())
	var size *int64
	err := pool.QueryRow(ctx, shadowBytesSQL, shape.Schema(), shadow, shape.Database()).Scan(&size)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("measure shadow %s.%s: %w", shape.Schema(), shadow, err)
	}
	if size == nil {
		return 0, nil
	}
	return *size, nil
}

// slotRetainedBytes is the WAL the route's slot retains, as the server
// measures it, or zero when that is not WAL this run will keep counting
// against its ceiling: no slot of the name, a slot that is not this
// database's logical slot, a lost slot — a resume replaces it — and a
// retained figure the server cannot measure.
func slotRetainedBytes(ctx context.Context, pool *pgxpool.Pool, name string) (int64, error) {
	status, found, err := decode.InspectSlot(ctx, pool, name)
	if err != nil {
		return 0, err
	}
	if !countsRetainedWAL(status, found) {
		return 0, nil
	}
	return status.Retained.Bytes, nil
}

// countsRetainedWAL reports whether a slot reading proves WAL the resumed
// run's slot holds on the volume.
func countsRetainedWAL(status decode.SlotStatus, found bool) bool {
	if !found || !status.Own {
		return false
	}
	if status.WALStatus == decode.WALStatusLost {
		return false
	}
	return status.Retained.Known && status.Retained.Bytes > 0
}
