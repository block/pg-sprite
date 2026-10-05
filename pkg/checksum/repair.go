package checksum

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/preflight"
)

// Repair is one chunk a pass recopied under DivergenceRepair: the
// difference it found, and what the recopy removed and put back. The recopy
// has committed; whether the chunk then read equal is the pass's result,
// not the Repair's — a chunk that did not is named by a RepairError
// alongside the Repairs that committed with it.
type Repair struct {
	// Mismatch is the difference the pass found before the repair.
	Mismatch Mismatch
	// Removed is the number of shadow rows the repair deleted from the chunk.
	Removed int64
	// Inserted is the number of source rows the repair copied back.
	Inserted int64
}

// repairAll recopies every differing chunk in one transaction, then reads
// each one again in a fresh snapshot, in key order, and stops at the first
// chunk whose recopy did not take. The repairs it returns are the ones that
// committed: none when the recopy failed, all of them with any error the
// rereads raise. Every transaction runs under the lock session's Bind
// context, as the pass did.
func (v *Verifier) repairAll(ctx context.Context, pool *pgxpool.Pool, mismatches []Mismatch) ([]Repair, error) {
	ctx, unbind := v.lock.Bind(ctx)
	defer unbind()
	sourceSQL, shadowSQL, err := v.digestStatements(ctx, pool)
	if err != nil {
		return nil, v.lostOr(err)
	}
	repairs, err := v.recopy(ctx, pool, mismatches)
	if err != nil {
		return nil, v.lostOr(err)
	}
	for _, repair := range repairs {
		if err := v.reread(ctx, pool, sourceSQL, shadowSQL, repair); err != nil {
			return repairs, v.lostOr(err)
		}
	}
	if lost := v.lockLost(); lost != nil {
		return repairs, lost
	}
	return repairs, nil
}

// recopy replaces the shadow's rows for every differing chunk in one
// guarded read-write transaction: delete every chunk's rows, then run the
// copier's chunk statement for every chunk, and commit the whole set
// together. Every delete runs before any insert because the source's
// unique indexes stand on the shadow too: when the source moved a unique
// value from a row in one differing chunk to a row in another while the
// shadow stood still, recopying chunk by chunk would insert the value's new
// row while the shadow still held its old one, and the index would refuse
// the recopy on every pass. One transaction also means a half-repaired
// shadow — rows gone and not yet put back — is never visible to another
// transaction. The recopy is the copier's own statement, not a copy of it,
// so a repair puts back exactly what a copy would have.
func (v *Verifier) recopy(ctx context.Context, pool *pgxpool.Pool, mismatches []Mismatch) ([]Repair, error) {
	tx, err := v.begin(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadWrite})
	if err != nil {
		return nil, err
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed; on a failure path the server aborts
		// the transaction with its session either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	repairs := make([]Repair, 0, len(mismatches))
	for _, mismatch := range mismatches {
		chunk := mismatch.Chunk
		removed, err := tx.Exec(ctx, v.repairSQL, chunk.Lower(), chunk.Upper())
		if err != nil {
			return nil, fmt.Errorf("clear chunk [%d, %d] of shadow %s.%s for repair: %w", chunk.Lower(), chunk.Upper(), v.shadow.Schema(), v.shadow.ShadowTable(), err)
		}
		repairs = append(repairs, Repair{Mismatch: mismatch, Removed: removed.RowsAffected()})
	}
	for i := range repairs {
		chunk := repairs[i].Mismatch.Chunk
		inserted, err := tx.Exec(ctx, v.copySQL, chunk.Lower(), chunk.Upper())
		if err != nil {
			return nil, fmt.Errorf("recopy chunk [%d, %d] of %s.%s into %s: %w", chunk.Lower(), chunk.Upper(), v.shadow.Schema(), v.shadow.SourceTable(), v.shadow.ShadowTable(), err)
		}
		repairs[i].Inserted = inserted.RowsAffected()
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit repair of %d chunks of %s.%s: %w", len(repairs), v.target.Schema(), v.target.Table(), err)
	}
	v.countRepaired(len(repairs))
	return repairs, nil
}

// reread digests the repaired chunk in a fresh snapshot and refuses a chunk
// that still differs. The reread is counted when its digest has committed,
// whatever it found: a chunk that still differs was reread all the same.
func (v *Verifier) reread(ctx context.Context, pool *pgxpool.Pool, sourceSQL, shadowSQL string, repair Repair) error {
	chunk := repair.Mismatch.Chunk
	source, shadow, err := v.digestChunk(ctx, pool, sourceSQL, shadowSQL, chunk.Lower(), chunk.Upper())
	if err != nil {
		return err
	}
	v.countReread()
	if source != shadow {
		// INV: CO-2
		return &RepairError{Repair: repair, After: Mismatch{Chunk: chunk, Source: source, Shadow: shadow}}
	}
	return nil
}

// repairSQL is the one statement every repair runs before the recopy:
// delete every shadow row whose key lies in the closed range [$1, $2]. The
// bounds are declared bigint as the copy statement's are, so the
// primary-key index serves the range.
func repairSQL(target preflight.CopySwapTarget, shadow copier.Shadow) string {
	return "DELETE FROM " + pgx.Identifier{shadow.Schema(), shadow.ShadowTable()}.Sanitize() +
		" WHERE " + pgx.Identifier{target.PKColumn()}.Sanitize() + " BETWEEN $1::bigint AND $2::bigint"
}
