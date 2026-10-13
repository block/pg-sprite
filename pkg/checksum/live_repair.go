package checksum

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/decode"
)

// repairLive recopies every confirmed divergence in one fenced transaction,
// waits for the catch-up to confirm past the recopy, and settles each
// repaired chunk again, in key order, stopping at the first that does not
// settle clean. The repairs it returns are the ones that committed: none
// when the recopy failed, all of them with any error the settling raises.
func (v *Verifier) repairLive(ctx context.Context, pool *pgxpool.Pool, mismatches []Mismatch, live LiveOptions) ([]Repair, error) {
	sourceSQL, shadowSQL, err := v.digestStatements(ctx, pool)
	if err != nil {
		return nil, err
	}
	repairs, recopied, err := v.recopyFenced(ctx, pool, mismatches)
	if err != nil {
		return nil, err
	}
	for _, repair := range repairs {
		if err := v.settleRepair(ctx, pool, sourceSQL, shadowSQL, repair, recopied, live); err != nil {
			return repairs, err
		}
	}
	return repairs, nil
}

// recopyFenced is the recopy for a shadow the stream is writing: the same
// delete-every-chunk-then-copy-every-chunk transaction, with the stream's
// flushes held off the shadow while it runs. SHARE MODE on the shadow waits
// behind a flush that is mid-write and keeps the next one waiting until the
// recopy commits, so no flush interleaves with the delete and the copy; the
// flushes that then land replay images the recopy has already put back, or
// changes that committed after it, and the shadow converges on the source
// as it does after any copy. The marker is written after the copy and
// before the commit: every source change the copy saw committed before it,
// so once the catch-up confirms past it the shadow holds the recopy and
// everything since. The wait for the lock is bounded by the transaction's
// lock_timeout, and a flush queued behind the recopy by its own.
func (v *Verifier) recopyFenced(ctx context.Context, pool *pgxpool.Pool, mismatches []Mismatch) ([]Repair, decode.LSN, error) {
	tx, err := v.begin(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadWrite})
	if err != nil {
		return nil, 0, err
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed; on a failure path the server aborts
		// the transaction with its session either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	// INV: LK-2
	if _, err := tx.Exec(ctx, v.fenceSQL); err != nil {
		return nil, 0, fmt.Errorf("fence the stream's flushes out of shadow %s.%s for repair: %w", v.shadow.Schema(), v.shadow.ShadowTable(), err)
	}
	repairs, err := v.recopyIn(ctx, tx, mismatches)
	if err != nil {
		return nil, 0, err
	}
	recopied, err := v.markPosition(ctx, tx)
	if err != nil {
		return nil, 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, 0, fmt.Errorf("commit repair of %d chunks of %s.%s: %w", len(repairs), v.target.Schema(), v.target.Table(), err)
	}
	v.countRepaired(len(repairs))
	return repairs, recopied, nil
}

// settleRepair reads a repaired chunk once the catch-up has confirmed past
// the recopy and, when the two sides still differ, settles it as the sweep
// settles a differing chunk. A chunk that settles clean is repaired; one
// that is a confirmed divergence after its repair is a RepairError (CO-2),
// and one that cannot be settled is an UnresolvedError.
func (v *Verifier) settleRepair(ctx context.Context, pool *pgxpool.Pool, sourceSQL, shadowSQL string, repair Repair, recopied decode.LSN, live LiveOptions) error {
	chunk := repair.Mismatch.Chunk
	// The recopy left no digest to compare the first reread with, so the
	// wait is for the recopy's own position and the first reread stands on
	// its own: equal is repaired, anything else is settled from there, with
	// that reread counted among the chunk's attempts.
	prior := observation{chunk: chunk, lsn: recopied}
	if err := v.awaitConfirmed(ctx, prior, 0, live); err != nil {
		return err
	}
	now, err := v.observe(ctx, pool, sourceSQL, shadowSQL, chunk)
	if err != nil {
		return err
	}
	v.countReread()
	const rereads = 1
	if now.source == now.shadow {
		return nil
	}
	result, last, err := v.settle(ctx, pool, sourceSQL, shadowSQL, now, rereads, live)
	if err != nil {
		return err
	}
	switch result {
	case verdictEqual, verdictSettled:
		return nil
	case verdictDiverged:
		// INV: CO-2
		return &RepairError{Repair: repair, After: Mismatch{Chunk: chunk, Source: last.source, Shadow: last.shadow}}
	default:
		return fmt.Errorf("%w (CO-2): settle returned verdict %d after repair", ErrInvariantViolation, result)
	}
}

// fenceSQL is the one statement a live repair runs before its deletes: a
// SHARE MODE lock on the shadow, which conflicts with the ROW EXCLUSIVE a
// flush holds while it writes and blocks none of the ACCESS SHARE readers.
func fenceSQL(shadow copier.Shadow) string {
	return "LOCK TABLE " + pgx.Identifier{shadow.Schema(), shadow.ShadowTable()}.Sanitize() + " IN SHARE MODE"
}
