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
	cp, resumable, err := ResumableRow(ctx, store, shape.Schema(), shape.Table())
	if err != nil {
		return preflight.CopySwapHeld{}, err
	}
	if !resumable {
		return preflight.CopySwapHeld{}, nil
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

// ResumableRow reads the table's checkpoint row and reports whether it
// records a run the next run resumes: a row in a phase this engine knows
// that is not terminal. The row-level outcomes Inspect reports — no row,
// no table, a table an older engine shaped, a row in another format or an
// unknown phase — are not resumes here; the run decides those itself under
// its lock. Any other error is returned. The read takes no lock, so it is
// a forecast for a caller deciding where to send a statement: a Runner
// reads the row again under the table's lock and decides from that.
func ResumableRow(ctx context.Context, store *checkpoint.Store, schema, table string) (checkpoint.Checkpoint, bool, error) {
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
