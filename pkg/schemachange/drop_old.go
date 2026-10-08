package schemachange

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// DropOldTable removes the retained source after a committed swap (D9), in
// one bounded transaction separate from the swap's, under SET LOCAL ROLE
// owner and only while the caller holds the table lock. It accepts only
// the SwappedTable proof Cutover or InspectSwapped minted and drops only
// the relation whose OID that proof recorded as the source — a different
// relation under the _old name is refused, not dropped. The drop is
// without CASCADE: the old table owns its renamed indexes and its old
// identity sequences, which go with it, and nothing the live table depends
// on. A caller that wants the old table kept for a rollback window simply
// does not call this.
func DropOldTable(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, swapped SwappedTable, opts Options) error {
	if err := opts.validate(); err != nil {
		return err
	}
	if swapped.table == "" {
		return refuse(CauseProofEmpty, nil, "swapped table proof is empty")
	}
	if err := requireTableLock(lock, swapped.Schema(), swapped.Table()); err != nil {
		return err
	}
	ctx, stop := lock.Bind(ctx)
	defer stop()
	if err := dropOldTable(ctx, pool, lock, swapped, opts); err != nil {
		return lockLossCause(lock, err)
	}
	return nil
}

func dropOldTable(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, swapped SwappedTable, opts Options) error {
	schema, old := swapped.Schema(), swapped.OldTable()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin old table drop: %w", err)
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed; on a failure path the server aborts
		// the transaction with its session either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	if err := setSession(ctx, tx, schema, swapped.Owner(), opts); err != nil {
		return err
	}
	if err := confirmTableLock(ctx, tx, lock); err != nil {
		return err
	}
	if err := lockOldTable(ctx, tx, schema, old); err != nil {
		return err
	}
	oid, err := resolveRelation(ctx, tx, schema, old)
	if err != nil {
		return err
	}
	// INV: ST-6
	if oid != swapped.OldOID() {
		return refuse(CauseRelationReplaced, nil, "old table %s.%s is relation %d, the swap retained %d", schema, old, oid, swapped.OldOID())
	}
	if _, err := tx.Exec(ctx, "DROP TABLE "+pgx.Identifier{schema, old}.Sanitize()); err != nil {
		return fmt.Errorf("drop old table %s.%s: %w", schema, old, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit old table drop: %w", err)
	}
	return nil
}

// lockOldTable takes ACCESS EXCLUSIVE on whatever bears the _old name,
// under the session's lock_timeout, before the OID is read. DROP TABLE
// resolves its name again once it holds its own lock, so a rename that
// commits while the drop waits would otherwise move the proven relation
// out from under the name and put another in its place; once the relation
// is locked it cannot be renamed, and the OID check and the drop see the
// same one (ST-6). Nothing under the name surfaces as the server's
// undefined_table error, reachable by SQLSTATE.
func lockOldTable(ctx context.Context, tx pgx.Tx, schema, old string) error {
	if _, err := tx.Exec(ctx, "LOCK TABLE "+pgx.Identifier{schema, old}.Sanitize()+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		return fmt.Errorf("lock old table %s.%s: %w", schema, old, err)
	}
	return nil
}
