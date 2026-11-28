package executor

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/progress"
)

// dropAttachProof takes the proof's CHECK off its table, whether NOT VALID
// or validated: the inverse of ensureAttachProof. It is idempotent: with
// nothing under the proof's name it drops nothing. A name held by a
// constraint that is not a CHECK is refused, and the constraint left as it
// was. Like the ADD, the DROP waits outside the lock queue, up to
// holderWait, then proves the table and decides again under its lock in one
// brief transaction, so a CHECK the name no longer resolves to is never
// dropped.
func dropAttachProof(ctx context.Context, pool *pgxpool.Pool, proof attachProof, holderWait time.Duration, tracker *progress.Tracker) error {
	if err := proof.validate(); err != nil {
		return err
	}
	// INV: LK-2 — the DROP takes ACCESS EXCLUSIVE, so it requests its lock
	// once no long-lived reader or writer holds the table, or once
	// holderWait runs out.
	if _, err := waitOutsideLockQueue(ctx, pool, proof.lockedTables(), holderWait, systemHolderWaitClock(), trackerHolderWaitReport(tracker)); err != nil {
		return err
	}
	tx := briefTx{pool: pool}
	err := tx.run(ctx, "drop the attach proof of "+proof.qualified(), catalogReadPath(), true,
		func(ctx context.Context, tx pgx.Tx) error {
			// INV: ST-7 — the table stays under its name, and its
			// constraints stay as read, until the transaction ends.
			if err := lockAndProveAttachProofTable(ctx, tx, proof); err != nil {
				return err
			}
			state, _, err := readAttachProof(ctx, tx, proof)
			if err != nil {
				return err
			}
			if state == attachProofAddNeeded {
				return nil
			}
			// INV: LK-2 — dropping a CHECK changes only the catalog, under
			// the pool's lock_timeout and statement_timeout.
			_, err = tx.Exec(ctx, proof.dropSQL())
			return err
		})
	if err != nil {
		return fmt.Errorf("drop attach proof %s on %s: %w", proof.name(), proof.qualified(), err)
	}
	return nil
}

func (p attachProof) dropSQL() string {
	return "ALTER TABLE " + pgx.Identifier{p.schema, p.table}.Sanitize() +
		" DROP CONSTRAINT " + pgx.Identifier{p.name()}.Sanitize()
}
