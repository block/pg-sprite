package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// ErrReplicaIdentityDrop means a native drop would remove what a replica
// identity index of the table, or of a table below it, is built on. The
// planner refuses such a drop from the facts it read; this is the same
// answer read again under the table's lock, so it fires when the identity
// moved onto the dropped object after planning. Nothing was dropped. The
// outcome is permanent: point REPLICA IDENTITY elsewhere, then re-plan.
var ErrReplicaIdentityDrop = errors.New("the drop removes what a replica identity index is built on")

// replicaIdentityDrops returns the operations of st that can remove what a
// replica identity index is built on when they run natively: a dropped
// column or constraint. A statement with none needs no guard.
func replicaIdentityDrops(st statement.Statement) ([]statement.Op, error) {
	if st.Kind() != statement.KindAlterTable {
		return nil, nil
	}
	ops, err := statement.ParseOps(st.SQL())
	if err != nil {
		return nil, fmt.Errorf("read the operations of %q: %w", st.SQL(), err)
	}
	var drops []statement.Op
	for _, op := range ops {
		if op.Kind == statement.OpDropColumn || op.Kind == statement.OpDropConstraint {
			drops = append(drops, op)
		}
	}
	return drops, nil
}

// refuseReplicaIdentityLoss locks st's table and every table below it in
// ACCESS EXCLUSIVE, the lock the drop takes on each of them anyway and the
// one ALTER TABLE ... REPLICA IDENTITY needs, then reads what their replica
// identity indexes are built on and refuses any of drops that would remove
// it. Holding the lock until the drop commits means the identity cannot
// move between this read and the drop. A lock that is not granted within
// the attempt's lock_timeout fails like the drop itself would.
func refuseReplicaIdentityLoss(ctx context.Context, tx pgx.Tx, st statement.Statement, drops []statement.Op) error {
	target := pgx.Identifier{st.Table()}
	if st.Schema() != "" {
		target = pgx.Identifier{st.Schema(), st.Table()}
	}
	if _, err := tx.Exec(ctx, "LOCK TABLE "+target.Sanitize()+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		return fmt.Errorf("lock %s to check its replica identity: %w", target.Sanitize(), err)
	}
	var oid uint32
	if err := tx.QueryRow(ctx, "SELECT $1::regclass::oid", target.Sanitize()).Scan(&oid); err != nil {
		return fmt.Errorf("resolve %s to check its replica identity: %w", target.Sanitize(), err)
	}
	keys, err := schemadiff.ReadReplicaIdentity(ctx, tx, oid)
	if err != nil {
		return fmt.Errorf("read the replica identity of %s: %w", target.Sanitize(), err)
	}
	for _, op := range drops {
		if keys.DroppedBy(op) {
			return fmt.Errorf("%w: %s on %s, which makes UPDATE and DELETE fail "+
				"while a publication publishes them; point REPLICA IDENTITY at another unique index first "+
				"(or DEFAULT, if a usable primary key remains after the drop), then re-plan",
				ErrReplicaIdentityDrop, op.Describe(), target.Sanitize())
		}
	}
	return nil
}
