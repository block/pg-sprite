package schemachange

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
)

// DropShadow removes the shadow an earlier build left behind, in one bounded
// transaction under SET LOCAL ROLE owner and only while the caller holds the
// table lock, so a drop can never race a build or a copy on the same table.
// It drops only a plain table owned by the source's owner under the shadow's
// name — anything else wearing that name is not this engine's shadow and is
// refused (ST-5) — and it drops without CASCADE: the shadow's identity
// defaults depend on the source's sequences, never the other way round, so
// the shadow owns nothing the source needs and a cascade could only reach
// objects the engine did not create. A missing shadow is ErrShadowNotFound.
func DropShadow(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, target preflight.CopySwapTarget, opts Options) error {
	if err := opts.validate(); err != nil {
		return err
	}
	if err := checkProof(target); err != nil {
		return err
	}
	if err := requireTableLock(lock, target); err != nil {
		return err
	}
	ctx, stop := lock.Bind(ctx)
	defer stop()
	if err := dropShadow(ctx, pool, lock, target, opts); err != nil {
		return lockLossCause(ctx, err)
	}
	return nil
}

func dropShadow(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, target preflight.CopySwapTarget, opts Options) error {
	shadow := ShadowName(target.Schema(), target.Table())
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin shadow drop: %w", err)
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed; on a failure path the server aborts
		// the transaction with its session either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	if err := setBuildSession(ctx, tx, target, opts); err != nil {
		return err
	}
	if err := confirmTableLock(ctx, tx, lock); err != nil {
		return err
	}
	if _, err := resolveShadow(ctx, tx, target.Schema(), shadow, target.OwnerRole()); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "DROP TABLE "+pgx.Identifier{target.Schema(), shadow}.Sanitize()); err != nil {
		return fmt.Errorf("drop shadow %s.%s: %w", target.Schema(), shadow, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit shadow drop: %w", err)
	}
	return nil
}
