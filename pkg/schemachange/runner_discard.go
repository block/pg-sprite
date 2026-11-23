package schemachange

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/preflight"
)

// Discarded is what one Runner.Discard dropped for a table. Each field
// names a leftover the call found and dropped; an empty or false field is
// a kind of leftover it found none of. Nothing reports a call that found
// nothing to discard.
type Discarded struct {
	// Schema and Table name the table whose run was discarded.
	Schema, Table string
	// RowPhase is the phase of the checkpoint row the call deleted, the
	// zero Phase when no row recorded a run.
	RowPhase checkpoint.Phase
	// Shadow is the name of the shadow table dropped.
	Shadow string
	// Slot and Publication name the logical replication slot and the
	// publication dropped: the hold a live run keeps on the source.
	Slot, Publication string
	// WriteFence reports that the write fence was taken off the source:
	// the hold a quiesced run keeps on it.
	WriteFence bool
}

// Nothing reports that the call found nothing to discard: no checkpoint
// row, no shadow, no slot or publication, no write fence. It is the
// answer to a discard called again after one that completed.
func (d Discarded) Nothing() bool {
	return d.RowPhase == 0 && d.Shadow == "" && d.Slot == "" && d.Publication == "" && !d.WriteFence
}

// Discard ends the copy-and-swap run on the shape's table without running
// the change: whatever a stopped run left — the shadow, its hold on the
// source (a live run's replication slot and publication, or a quiesced
// run's write fence), and its checkpoint row — is dropped, and the table
// is left as if no run had started. It takes the table's lock the way Run
// does and never waits for it: a run in progress holds the lock for as
// long as it runs, so Discard is refused with the
// *dbconn.TableLockHeldError naming the holder and touches nothing. A
// caller that means to cancel a run in progress stops that run first,
// then discards.
//
// With the lock held the row is read and the leftovers found from the
// catalog, and Discard dispatches on the row:
//
//   - A row in a phase before its swap committed is discarded as
//     RunOptions.Restart discards it — the shadow first, then the slot or
//     the fence, then the row — so a discard that stops partway leaves a
//     row that still records what stands, and a second Discard finishes
//     it. A row naming a slot other than the table's derived name is
//     refused as resume-row-mismatch before anything is dropped (ST-3).
//   - A cutover row is answered by the catalog first (LK-4): a swap that
//     committed is refused as restart-swap-committed, and nothing is
//     dropped — the shadow already bears the source's name and the source
//     is the retained table under its derived _old name, so the change is
//     past the point of discarding; a Run of the same statement finishes
//     it. A swap that rolled back is discarded like any other stopped run.
//   - A done row records a change that completed and is refused as
//     resume-row-terminal: there is no run to discard.
//   - No row: a shadow, a write fence, or a slot and publication of the
//     table's derived names that no row records are dropped. The slot is
//     dropped as the orphan reaper drops one (checkpoint.Store.
//     ReapOrphanSlots), under the engine's lock on its name and without
//     waiting for a holder, so a held slot is refused with an error
//     wrapping decode.ErrSlotActive and left; a slot of the name that is
//     not this database's logical slot is not the engine's and is left.
//     Nothing found is the zero Discarded with Schema and Table set, whose
//     Nothing is true, and no error.
//
// A slot is dropped over a replication connection, so a discard that
// finds one refuses a shape whose privilege proof did not verify
// replication access, before dropping anything, with an error wrapping
// preflight.ErrCopySwapProofMismatch. Discard writes the checkpoint table
// into existence as Run does (checkpoint.Store.Ensure). Every error is
// returned with the zero Discarded, with a lost table lock reported as
// shadow-lock-lost; what an interrupted call dropped is gone, and the
// next call reports only what it drops itself.
func (r *Runner) Discard(ctx context.Context, pool *pgxpool.Pool, shape preflight.CopySwapShape) (discarded Discarded, err error) {
	if err := checkProof(shape); err != nil {
		return Discarded{}, err
	}
	schema, table := shape.Schema(), shape.Table()
	// INV: LK-1 — the discard holds the run's lock, so it cannot overlap a
	// run on the table; a held lock is refused at once, never waited for.
	lock, err := dbconn.AcquireTableLock(ctx, r.cfg, schema, table)
	if err != nil {
		return Discarded{}, fmt.Errorf("discard %s.%s: %w", schema, table, err)
	}
	defer func() {
		// As Run's release: a lost lock closes its own session, and the
		// release outlives the caller's context but is bounded.
		if lock.Err() == nil {
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbconn.DefaultConnectTimeout)
			defer cancel()
			err = errors.Join(err, lock.Release(releaseCtx))
		}
	}()
	discarded, err = r.discard(ctx, pool, lock, shape)
	if err != nil {
		return Discarded{}, lockLossCause(lock, fmt.Errorf("discard %s.%s: %w", schema, table, err))
	}
	return discarded, nil
}

// discard reads the table's row under the lock and discards what it
// records, or what no row records.
func (r *Runner) discard(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, shape preflight.CopySwapShape) (Discarded, error) {
	if err := r.store.Ensure(ctx); err != nil {
		return Discarded{}, err
	}
	cp, err := r.store.Inspect(ctx, shape.Schema(), shape.Table())
	switch {
	case errors.Is(err, checkpoint.ErrNotFound):
		return r.discardLeftovers(ctx, pool, lock, shape)
	case err != nil:
		return Discarded{}, err
	case cp.Phase.Terminal():
		return Discarded{}, refuse(CauseResumeTerminal, nil, "checkpoint row for %s.%s is in phase %s: the run it records has ended, so there is no run to discard", cp.Schema, cp.Table, cp.Phase)
	default:
		return r.discardStoppedRun(ctx, pool, lock, shape, cp)
	}
}

// discardStoppedRun discards the run the row cp records, as a restart
// discards it, and reports what was there to drop.
func (r *Runner) discardStoppedRun(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, shape preflight.CopySwapShape, cp checkpoint.Checkpoint) (Discarded, error) {
	if err := r.checkDiscardable(ctx, pool, lock, shape, cp); err != nil {
		return Discarded{}, err
	}
	found, err := r.findLeftovers(ctx, pool, lock, shape)
	if err != nil {
		return Discarded{}, err
	}
	discarded := Discarded{Schema: cp.Schema, Table: cp.Table, RowPhase: cp.Phase}
	if found.shadow {
		discarded.Shadow = ShadowName(shape.Schema(), shape.Table())
	}
	// The hold is the one the row records, as discardHold releases it.
	if cp.SlotName == "" {
		discarded.WriteFence = found.fence
	} else {
		if err := requireReplication(shape, found); err != nil {
			return Discarded{}, err
		}
		discarded.Slot, discarded.Publication = found.names(shape)
	}
	if err := r.discardRecorded(ctx, pool, lock, shape, cp); err != nil {
		return Discarded{}, err
	}
	return discarded, nil
}

// discardLeftovers drops what a run left that no checkpoint row records:
// the shadow and the write fence as a restart drops them, and a slot and
// publication of the table's derived names as the reaper drops an orphan.
func (r *Runner) discardLeftovers(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, shape preflight.CopySwapShape) (Discarded, error) {
	found, err := r.findLeftovers(ctx, pool, lock, shape)
	if err != nil {
		return Discarded{}, err
	}
	discarded := Discarded{Schema: shape.Schema(), Table: shape.Table(), WriteFence: found.fence}
	if found.shadow {
		discarded.Shadow = ShadowName(shape.Schema(), shape.Table())
	}
	discarded.Slot, discarded.Publication = found.names(shape)
	if discarded.Nothing() {
		return discarded, nil
	}
	if err := requireReplication(shape, found); err != nil {
		return Discarded{}, err
	}
	if found.shadow || found.fence {
		if err := r.discardUnrecorded(ctx, pool, lock, shape); err != nil {
			return Discarded{}, err
		}
	}
	if found.slot || found.publication {
		if err := r.discardOrphanSlot(ctx, pool, shape); err != nil {
			return Discarded{}, err
		}
	}
	return discarded, nil
}

// discardOrphanSlot drops the slot and publication of the table's derived
// name that no checkpoint row records, the way the reaper drops an orphan:
// under the engine's lock on the slot's name, after reading again that no
// row names it, and without waiting for a holder.
func (r *Runner) discardOrphanSlot(ctx context.Context, pool *pgxpool.Pool, shape preflight.CopySwapShape) error {
	name := shape.DecodingName()
	return r.store.WithSlotLock(ctx, r.cfg, name, func(ctx context.Context) error {
		cp, err := r.store.Inspect(ctx, shape.Schema(), shape.Table())
		switch {
		case errors.Is(err, checkpoint.ErrNotFound):
		case err != nil:
			return err
		default:
			// INV: ST-3 — a slot a row names is the row's; the table lock
			// keeps any run from saving one, so a row here is a write
			// that bypassed the lock.
			return fmt.Errorf("%w (ST-3): a checkpoint row for %s.%s in phase %s appeared while the discard held the table lock", ErrInvariantViolation, cp.Schema, cp.Table, cp.Phase)
		}
		// INV: ST-3 — an unrecorded slot is dropped only while no process
		// holds it; a held slot is its holder's and is left as found.
		if err := decode.DropIdleSlot(ctx, r.cfg, pool, name); err != nil {
			return fmt.Errorf("discard unrecorded slot %s: %w", name, err)
		}
		return nil
	})
}

// requireReplication refuses a discard that would drop a slot under a
// shape whose privilege proof did not verify replication access: the drop
// runs on a replication connection, and refusing first keeps a discard
// from dropping the shadow and then stopping at the slot.
func requireReplication(shape preflight.CopySwapShape, found leftovers) error {
	if !found.slot || shape.LogicalDecoding() {
		return nil
	}
	return fmt.Errorf("%w: discarding %s.%s drops replication slot %s, but the shape's privilege proof did not verify replication access", preflight.ErrCopySwapProofMismatch, shape.Schema(), shape.Table(), shape.DecodingName())
}

// leftovers is what the catalog shows under the names a run on the table
// makes: the shadow, the write fence on the source, and the slot and
// publication of the table's derived decoding name.
type leftovers struct {
	shadow, fence, slot, publication bool
}

// names returns the slot and publication names found, empty for each the
// catalog does not show.
func (l leftovers) names(shape preflight.CopySwapShape) (slot, publication string) {
	if l.slot {
		slot = shape.DecodingName()
	}
	if l.publication {
		publication = shape.DecodingName()
	}
	return slot, publication
}

// writeFencePresentSQL reports whether the source carries a trigger under
// the fence's name, of whatever shape: the drop removes it by name. Every
// catalog name is qualified (CO-9).
const writeFencePresentSQL = `
SELECT EXISTS (
	SELECT 1
	FROM pg_catalog.pg_trigger tg
	JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) tg.tgrelid
	JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
	WHERE n.nspname OPERATOR(pg_catalog.=) $1
	  AND c.relname OPERATOR(pg_catalog.=) $2
	  AND tg.tgname OPERATOR(pg_catalog.=) $3
	  AND NOT tg.tgisinternal
)`

// publicationPresentSQL reports whether a publication bears the name.
// Every catalog name is qualified (CO-9).
const publicationPresentSQL = `
SELECT EXISTS (
	SELECT 1
	FROM pg_catalog.pg_publication p
	WHERE p.pubname OPERATOR(pg_catalog.=) $1
)`

// findLeftovers reads what stands under the table's derived names, in one
// bounded read-only transaction that confirms the table lock (LK-1), and
// then the slot, which only a slot of the engine's in this database counts
// as: a slot of the name another database owns is not this table's run's.
// Under the lock no run on the table can make or drop any of them, so the
// reading holds until the discard acts on it.
func (r *Runner) findLeftovers(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, shape preflight.CopySwapShape) (leftovers, error) {
	found, err := r.findRelationLeftovers(ctx, pool, lock, shape)
	if err != nil {
		return leftovers{}, err
	}
	status, slotFound, err := decode.InspectSlot(ctx, pool, shape.DecodingName())
	if err != nil {
		return leftovers{}, fmt.Errorf("inspect slot %s: %w", shape.DecodingName(), err)
	}
	found.slot = slotFound && status.Own
	return found, nil
}

// findRelationLeftovers reads the shadow, the fence, and the publication.
func (r *Runner) findRelationLeftovers(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, shape preflight.CopySwapShape) (leftovers, error) {
	schema, table := shape.Schema(), shape.Table()
	ctx, stop := lock.Bind(ctx)
	defer stop()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return leftovers{}, fmt.Errorf("begin leftover read for %s.%s: %w", schema, table, err)
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed; on a failure path the server aborts
		// the transaction with its session either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	if err := setBudgets(ctx, tx, r.opts.Shadow); err != nil {
		return leftovers{}, err
	}
	if err := pinCatalogSearchPath(ctx, tx); err != nil {
		return leftovers{}, err
	}
	if err := confirmTableLock(ctx, tx, lock); err != nil {
		return leftovers{}, lockLossCause(lock, err)
	}
	var found leftovers
	if _, found.shadow, err = lookupRelation(ctx, tx, schema, ShadowName(schema, table)); err != nil {
		return leftovers{}, err
	}
	if err := tx.QueryRow(ctx, writeFencePresentSQL, schema, table, WriteFenceTrigger).Scan(&found.fence); err != nil {
		return leftovers{}, fmt.Errorf("read write fence on %s.%s: %w", schema, table, err)
	}
	if err := tx.QueryRow(ctx, publicationPresentSQL, shape.DecodingName()).Scan(&found.publication); err != nil {
		return leftovers{}, fmt.Errorf("read publication %s: %w", shape.DecodingName(), err)
	}
	if err := tx.Commit(ctx); err != nil {
		return leftovers{}, fmt.Errorf("commit leftover read for %s.%s: %w", schema, table, err)
	}
	return found, nil
}
