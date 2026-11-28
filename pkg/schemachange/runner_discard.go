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
//   - A row in a phase before its swap committed, or a row a caller
//     marked failed, is discarded as RunOptions.Restart discards it — the
//     shadow first, then the slot or the fence, then the row — so a
//     discard that stops partway leaves a row that still records what
//     stands, and a second Discard finishes it. The slot is dropped with
//     decode.DropSlot, which waits for a walsender holding it, bounded
//     only by ctx, so a caller passes a deadline. A row naming a slot
//     other than the table's derived name is refused as
//     resume-row-mismatch before anything is dropped (ST-3).
//   - A cutover row is answered by the catalog first (LK-4): a swap that
//     committed is refused as restart-swap-committed, and nothing is
//     dropped — the shadow already bears the source's name and the source
//     is the retained table under its derived _old name, so the change is
//     past the point of discarding; a Run of the same statement finishes
//     it. A swap that rolled back is discarded like any other stopped run.
//   - A done row records a change that completed and is refused as
//     resume-row-terminal: there is no run to discard.
//   - No row: a shadow, a write fence, or a slot and publication of the
//     table's derived names that no row records are dropped, the shadow
//     and the fence first. The slot is dropped as the orphan reaper drops
//     one (checkpoint.Store.ReapOrphanSlots): under the engine's lock on
//     its name, only once a read of every table's rows shows that no
//     row in a non-terminal phase names it, and without waiting for a
//     holder. The slot's name is a hash of the table's, so another
//     table's run can own a slot of the same name; that slot is refused
//     as resume-slot-foreign and left. A held slot is refused with an
//     error wrapping decode.ErrSlotActive and left; a slot of the name
//     that is not this database's logical slot is not the engine's and is
//     left. Both refusals are first judged before anything is dropped,
//     from the catalog reading that found the leftovers and a read of
//     every table's rows, so a slot another table's row names or a
//     process holds when the discard starts is refused with the shadow
//     and the fence still standing. Those readings are not under the
//     slot's lock, so both are judged again at the slot: a row saved or a
//     holder attached in between is refused there, after the shadow and
//     the fence are gone, and reported beside them. Nothing found is the
//     zero Discarded with Schema and Table set, whose Nothing is true,
//     and no error.
//
// A slot is dropped over a replication connection, so a discard that
// finds one refuses a shape whose privilege proof did not verify
// replication access, before dropping anything, with an error wrapping
// preflight.ErrCopySwapProofMismatch. Discard writes the checkpoint table
// into existence as Run does (checkpoint.Store.Ensure). An error is
// returned beside what the call dropped before it, with Schema and Table
// set: each leftover whose drop finished is named, and one whose drop the
// error interrupted is not, though part of it may be gone (a slot dropped
// before its publication). RowPhase is set only once the row is deleted.
// A lost table lock is reported as shadow-lock-lost, and a nil pool as
// ErrInvariantViolation before the lock is taken.
//
// The table lock is released before Discard returns. A release that fails
// after a discard that completed is returned as ErrLockReleaseFailed
// wrapping the release's error, beside a Discarded that names everything
// dropped: the discard is done and only the release failed. A release
// that fails after a discard that itself failed is joined with that
// error without the sentinel. The lock is a session-level advisory lock:
// the release closes the lock's session whether or not its unlock
// succeeded, and PostgreSQL lets go of a session's advisory locks when
// the session ends, so the lock is free once the server sees the session
// gone; until then another Discard or Run of the table is refused with
// the *dbconn.TableLockHeldError, never left waiting.
func (r *Runner) Discard(ctx context.Context, pool *pgxpool.Pool, shape preflight.CopySwapShape) (discarded Discarded, err error) {
	if err := checkProof(shape); err != nil {
		return Discarded{}, err
	}
	if pool == nil {
		return Discarded{}, fmt.Errorf("%w: a discard requires a pool", ErrInvariantViolation)
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
			err = joinDiscardRelease(err, schema, table, lock.Release(releaseCtx))
		}
	}()
	discarded, err = r.discard(ctx, pool, lock, shape)
	if err != nil {
		return discarded, lockLossCause(lock, fmt.Errorf("discard %s.%s: %w", schema, table, err))
	}
	return discarded, nil
}

// ErrLockReleaseFailed reports a Runner.Discard that completed — everything
// its Discarded names is dropped, and nothing it found is left — whose
// release of the table lock afterwards failed. It wraps the release's own
// error. A discard that itself failed returns its own error joined with
// the release's, without this sentinel, so errors.Is(err,
// ErrLockReleaseFailed) never makes a partial discard read as complete.
var ErrLockReleaseFailed = errors.New("discard completed; releasing the table lock failed")

// joinDiscardRelease folds the table lock's release error into the
// discard's outcome: a release failure after a discard that completed is
// ErrLockReleaseFailed, and one after a discard that failed is joined
// with the discard's error as it stands.
func joinDiscardRelease(discardErr error, schema, table string, releaseErr error) error {
	if releaseErr == nil {
		return discardErr
	}
	if discardErr != nil {
		return errors.Join(discardErr, fmt.Errorf("release table lock for %s.%s: %w", schema, table, releaseErr))
	}
	return fmt.Errorf("discard %s.%s: %w: %w", schema, table, ErrLockReleaseFailed, releaseErr)
}

// discard reads the table's row under the lock and discards what it
// records, or what no row records, reporting what it dropped beside any
// error.
func (r *Runner) discard(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, shape preflight.CopySwapShape) (Discarded, error) {
	none := Discarded{Schema: shape.Schema(), Table: shape.Table()}
	if err := r.store.Ensure(ctx); err != nil {
		return none, err
	}
	cp, err := r.store.Inspect(ctx, shape.Schema(), shape.Table())
	switch {
	case errors.Is(err, checkpoint.ErrNotFound):
		return r.discardLeftovers(ctx, pool, lock, shape)
	case err != nil:
		return none, err
	case cp.Phase == checkpoint.PhaseDone:
		return none, refuse(CauseResumeTerminal, nil, "checkpoint row for %s.%s is in phase %s: the change it records completed, so there is no run to discard", cp.Schema, cp.Table, cp.Phase)
	default:
		return r.discardStoppedRun(ctx, pool, lock, shape, cp)
	}
}

// discardStoppedRun discards the run the row cp records, as a restart
// discards it, and reports what it dropped.
func (r *Runner) discardStoppedRun(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, shape preflight.CopySwapShape, cp checkpoint.Checkpoint) (Discarded, error) {
	discarded := Discarded{Schema: cp.Schema, Table: cp.Table}
	if err := r.checkDiscardable(ctx, pool, lock, shape, cp); err != nil {
		return discarded, err
	}
	found, err := r.findLeftovers(ctx, pool, lock, shape)
	if err != nil {
		return discarded, err
	}
	// The hold is the one the row records, as discardHold releases it.
	if cp.SlotName != "" {
		if err := requireReplication(shape, found); err != nil {
			return discarded, err
		}
	}
	done, err := r.discardRecorded(ctx, pool, lock, shape, cp)
	if done.shadow && found.shadow {
		discarded.Shadow = ShadowName(shape.Schema(), shape.Table())
	}
	if done.hold && cp.SlotName == "" {
		discarded.WriteFence = found.fence
	}
	if done.hold && cp.SlotName != "" {
		discarded.Slot, discarded.Publication = found.names(shape)
	}
	if done.row {
		discarded.RowPhase = cp.Phase
	}
	return discarded, err
}

// discardLeftovers drops what a run left that no checkpoint row records:
// the shadow and the write fence as a restart drops them, and a slot and
// publication of the table's derived names as the reaper drops an orphan.
// It reports what it dropped beside any error.
func (r *Runner) discardLeftovers(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, shape preflight.CopySwapShape) (Discarded, error) {
	discarded := Discarded{Schema: shape.Schema(), Table: shape.Table()}
	found, err := r.findLeftovers(ctx, pool, lock, shape)
	if err != nil {
		return discarded, err
	}
	if err := requireReplication(shape, found); err != nil {
		return discarded, err
	}
	if err := r.refuseUndroppableOrphan(ctx, shape, found); err != nil {
		return discarded, err
	}
	if found.shadow {
		if err := r.discardShadow(ctx, pool, lock, shape); err != nil {
			return discarded, err
		}
		discarded.Shadow = ShadowName(shape.Schema(), shape.Table())
	}
	if found.fence {
		if err := dropWriteFenceOf(ctx, pool, lock, shape, r.opts.Shadow); err != nil {
			return discarded, err
		}
		discarded.WriteFence = true
	}
	if found.slot || found.publication {
		if err := r.discardOrphanSlot(ctx, pool, shape); err != nil {
			return discarded, err
		}
		discarded.Slot, discarded.Publication = found.names(shape)
	}
	return discarded, nil
}

// discardOrphanSlot drops the slot and publication of the table's derived
// name that no checkpoint row records, the way the reaper drops an orphan:
// under the engine's lock on the slot's name, after reading every table's
// rows again for one that names it, and without waiting for a holder.
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
		owned, err := r.store.LiveRowNamesSlot(ctx, name)
		if err != nil {
			return err
		}
		if owned {
			// INV: ST-3 — the slot's name is a hash, so another table's
			// run can own the same name; a slot a live row of any table
			// names is that run's and is left, as the reaper leaves it.
			return foreignOrphanSlot(shape)
		}
		// INV: ST-3 — an unrecorded slot is dropped only while no process
		// holds it; a held slot is its holder's and is left as found.
		if err := decode.DropIdleSlot(ctx, r.cfg, pool, name); err != nil {
			return fmt.Errorf("discard unrecorded slot %s: %w", name, err)
		}
		return nil
	})
}

// refuseUndroppableOrphan refuses, before anything is dropped, a slot or
// publication of the table's derived name that discardOrphanSlot would
// refuse: one another table's live row names, or a slot a process holds.
// Both readings are taken without the slot's lock, so they only keep a
// refusal the catalog already shows from arriving after the shadow and
// the fence are gone; discardOrphanSlot still judges both again, the row
// under the slot's lock and the holder at the drop itself, so a row saved
// or a holder attached after this check is refused there.
func (r *Runner) refuseUndroppableOrphan(ctx context.Context, shape preflight.CopySwapShape, found leftovers) error {
	if !found.slot && !found.publication {
		return nil
	}
	name := shape.DecodingName()
	owned, err := r.store.LiveRowNamesSlot(ctx, name)
	if err != nil {
		return err
	}
	if owned {
		// INV: ST-3 — another table's live row names the slot: it is that
		// run's, and the discard drops nothing of this table's either.
		return foreignOrphanSlot(shape)
	}
	if found.held {
		// INV: ST-3 — a held slot is its holder's; the discard refuses it
		// and drops nothing, rather than drop the shadow and stop at it.
		return fmt.Errorf("discard unrecorded slot %s: %w: held by backend %d", name, decode.ErrSlotActive, found.holder)
	}
	return nil
}

// foreignOrphanSlot is the resume-slot-foreign refusal of a slot of the
// table's derived name that another table's live row names.
func foreignOrphanSlot(shape preflight.CopySwapShape) error {
	name := shape.DecodingName()
	foreign := &decode.ForeignStateError{Object: decode.ForeignObjectSlot, Name: name,
		Detail: "a checkpoint row of another table names it"}
	return refuse(CauseResumeSlotForeign, []error{foreign}, "slot %s of %s.%s's derived name is another table's run's", name, shape.Schema(), shape.Table())
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
// publication of the table's derived decoding name. held reports that a
// process held the slot when it was read, and holder names that process.
type leftovers struct {
	shadow, fence, slot, publication bool
	held                             bool
	holder                           int32
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
	found.held = found.slot && status.Active
	found.holder = status.ActivePID
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
