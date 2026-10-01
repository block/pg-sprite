package schemachange

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// swapOnce is one attempt at the swap transaction: lock, drain, re-gate,
// rename, hand off, recheck, commit. Any error leaves the transaction
// rolled back on the server unless the connection broke around COMMIT,
// which the caller resolves by inspection.
func swapOnce(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, ready CutoverReady, drain DrainFunc, opts Options) (SwappedTable, error) {
	built := ready.built
	tx, err := pool.Begin(ctx)
	if err != nil {
		return SwappedTable{}, fmt.Errorf("begin cutover: %w", err)
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed; on a failure path the server aborts
		// the transaction with its session either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	if err := setGateSession(ctx, tx, built, opts); err != nil {
		return SwappedTable{}, err
	}
	if err := confirmTableLock(ctx, tx, lock); err != nil {
		return SwappedTable{}, err
	}
	if err := lockForSwap(ctx, tx, built); err != nil {
		return SwappedTable{}, err
	}
	if drain != nil {
		if err := drain(ctx, tx); err != nil {
			return SwappedTable{}, fmt.Errorf("drain captured changes into %s.%s: %w", built.Schema(), built.ShadowTable(), err)
		}
	}
	// The pairing the swap renames by is the one read under this lock, not
	// the gate's earlier read: the checklist refuses any drift in between,
	// so when it passes the two agree, and the lock makes this one final.
	fresh, err := gateCutoverTx(ctx, tx, built, ready.verified)
	if err != nil {
		return SwappedTable{}, err
	}
	if err := renameForSwap(ctx, tx, fresh); err != nil {
		return SwappedTable{}, err
	}
	if err := handOffSequences(ctx, tx, fresh); err != nil {
		return SwappedTable{}, err
	}
	if err := confirmSwap(ctx, tx, fresh); err != nil {
		return SwappedTable{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SwappedTable{}, fmt.Errorf("commit cutover of %s.%s: %w", built.Schema(), built.SourceTable(), err)
	}
	return newSwappedTable(fresh, 0), nil
}

// lockForSwap takes ACCESS EXCLUSIVE on the source and the shadow in one
// statement, bounded by the transaction's lock_timeout (LK-2). The source
// lock excludes every reader and writer for the rest of the transaction;
// the shadow lock excludes a straggling copier or applier connection.
func lockForSwap(ctx context.Context, tx pgx.Tx, built BuiltShadow) error {
	source := pgx.Identifier{built.Schema(), built.SourceTable()}.Sanitize()
	shadow := pgx.Identifier{built.Schema(), built.ShadowTable()}.Sanitize()
	// INV: LK-2
	if _, err := tx.Exec(ctx, "LOCK TABLE "+source+", "+shadow+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		return fmt.Errorf("lock %s.%s for cutover: %w", built.Schema(), built.SourceTable(), err)
	}
	return nil
}

// renameForSwap performs the D8 rename sequence: every source dependent to
// its derived _old name, the source to its _old name, the shadow to the
// source's name, and each paired shadow dependent to the name its source
// partner just gave up. ALTER INDEX … RENAME carries a backing constraint's
// name with the index. An unpaired shadow dependent keeps its
// shadow-derived name; an unpaired source dependent ends with the old table.
func renameForSwap(ctx context.Context, tx pgx.Tx, ready CutoverReady) error {
	built := ready.built
	schema, source := built.Schema(), built.SourceTable()
	for _, name := range ready.indexes.sourceNames() {
		if err := renameIndex(ctx, tx, schema, name, OldDependentName(schema, source, name)); err != nil {
			return err
		}
	}
	for _, name := range ready.statistics.sourceNames() {
		if err := renameStatistics(ctx, tx, schema, name, OldDependentName(schema, source, name)); err != nil {
			return err
		}
	}
	for _, id := range built.IdentityColumns() {
		if err := renameSequence(ctx, tx, id.SequenceSchema, id.SequenceName, OldDependentName(schema, source, id.SequenceName)); err != nil {
			return err
		}
	}
	if err := renameTable(ctx, tx, schema, source, OldName(schema, source)); err != nil {
		return err
	}
	if err := renameTable(ctx, tx, schema, built.ShadowTable(), source); err != nil {
		return err
	}
	for _, pair := range ready.indexes.Pairs {
		if err := renameIndex(ctx, tx, schema, pair.ShadowName, pair.SourceName); err != nil {
			return err
		}
	}
	for _, pair := range ready.statistics.Pairs {
		if err := renameStatistics(ctx, tx, schema, pair.ShadowName, pair.SourceName); err != nil {
			return err
		}
	}
	return nil
}

func renameTable(ctx context.Context, tx pgx.Tx, schema, from, to string) error {
	sql := "ALTER TABLE " + pgx.Identifier{schema, from}.Sanitize() + " RENAME TO " + pgx.Identifier{to}.Sanitize()
	if _, err := tx.Exec(ctx, sql); err != nil {
		return fmt.Errorf("rename table %s.%s to %s: %w", schema, from, to, err)
	}
	return nil
}

func renameIndex(ctx context.Context, tx pgx.Tx, schema, from, to string) error {
	sql := "ALTER INDEX " + pgx.Identifier{schema, from}.Sanitize() + " RENAME TO " + pgx.Identifier{to}.Sanitize()
	if _, err := tx.Exec(ctx, sql); err != nil {
		return fmt.Errorf("rename index %s.%s to %s: %w", schema, from, to, err)
	}
	return nil
}

func renameStatistics(ctx context.Context, tx pgx.Tx, schema, from, to string) error {
	sql := "ALTER STATISTICS " + pgx.Identifier{schema, from}.Sanitize() + " RENAME TO " + pgx.Identifier{to}.Sanitize()
	if _, err := tx.Exec(ctx, sql); err != nil {
		return fmt.Errorf("rename statistics %s.%s to %s: %w", schema, from, to, err)
	}
	return nil
}

func renameSequence(ctx context.Context, tx pgx.Tx, schema, from, to string) error {
	sql := "ALTER SEQUENCE " + pgx.Identifier{schema, from}.Sanitize() + " RENAME TO " + pgx.Identifier{to}.Sanitize()
	if _, err := tx.Exec(ctx, sql); err != nil {
		return fmt.Errorf("rename sequence %s.%s to %s: %w", schema, from, to, err)
	}
	return nil
}
