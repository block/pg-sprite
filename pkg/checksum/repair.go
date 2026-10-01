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
// difference it found, and what the recopy removed and put back. The chunk
// was read again after the recopy and found equal; a chunk that was not is
// a RepairError, not a Repair.
type Repair struct {
	// Mismatch is the difference the pass found before the repair.
	Mismatch Mismatch
	// Removed is the number of shadow rows the repair deleted from the chunk.
	Removed int64
	// Inserted is the number of source rows the repair copied back.
	Inserted int64
}

// repairAll recopies every differing chunk in key order, reading each one
// again after its recopy, and stops at the first chunk whose recopy did not
// take. Every transaction runs under the lock session's Bind context, as
// the pass did.
func (v *Verifier) repairAll(ctx context.Context, pool *pgxpool.Pool, mismatches []Mismatch) ([]Repair, error) {
	ctx, unbind := v.lock.Bind(ctx)
	defer unbind()
	sourceSQL, shadowSQL, err := v.digestStatements(ctx, pool)
	if err != nil {
		return nil, v.lostOr(err)
	}
	repairs := make([]Repair, 0, len(mismatches))
	for _, mismatch := range mismatches {
		repair, err := v.repairChunk(ctx, pool, mismatch)
		if err != nil {
			return nil, v.lostOr(err)
		}
		if err := v.reread(ctx, pool, sourceSQL, shadowSQL, repair); err != nil {
			return nil, v.lostOr(err)
		}
		repairs = append(repairs, repair)
	}
	if lost := v.lockLost(); lost != nil {
		return nil, lost
	}
	return repairs, nil
}

// repairChunk replaces the shadow's rows for one chunk with the source's in
// one guarded read-write transaction: delete every shadow row in the range,
// then run the copier's own chunk statement. The two statements commit
// together, so a half-repaired chunk — rows gone and not yet put back — is
// never visible to another transaction. The recopy is the copier's
// statement, not a copy of it, so a repair puts back exactly what a copy
// would have.
func (v *Verifier) repairChunk(ctx context.Context, pool *pgxpool.Pool, mismatch Mismatch) (Repair, error) {
	chunk := mismatch.Chunk
	tx, err := v.begin(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadWrite})
	if err != nil {
		return Repair{}, err
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed; on a failure path the server aborts
		// the transaction with its session either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	removed, err := tx.Exec(ctx, v.repairSQL, chunk.Lower(), chunk.Upper())
	if err != nil {
		return Repair{}, fmt.Errorf("clear chunk [%d, %d] of shadow %s.%s for repair: %w", chunk.Lower(), chunk.Upper(), v.shadow.Schema(), v.shadow.ShadowTable(), err)
	}
	inserted, err := copier.InsertChunk(ctx, tx, v.target, v.shadow, chunk)
	if err != nil {
		return Repair{}, fmt.Errorf("repair: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Repair{}, fmt.Errorf("commit repair of chunk [%d, %d] of %s.%s: %w", chunk.Lower(), chunk.Upper(), v.target.Schema(), v.target.Table(), err)
	}
	return Repair{Mismatch: mismatch, Removed: removed.RowsAffected(), Inserted: inserted}, nil
}

// reread digests the repaired chunk in a fresh snapshot and refuses a chunk
// that still differs.
func (v *Verifier) reread(ctx context.Context, pool *pgxpool.Pool, sourceSQL, shadowSQL string, repair Repair) error {
	chunk := repair.Mismatch.Chunk
	source, shadow, err := v.digestChunk(ctx, pool, sourceSQL, shadowSQL, chunk.Lower(), chunk.Upper())
	if err != nil {
		return err
	}
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
