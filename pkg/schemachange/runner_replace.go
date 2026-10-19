package schemachange

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/dbconn"
)

// replacesRow reports whether the run replaces cp rather than resuming
// or refusing it: only a done row, and only under ReplaceDone. A failed
// row is never replaced — nothing this Runner writes puts a row there, so
// it is another engine's to clear — and a row in any other phase records
// a run still under way, which resumes.
func (r *Runner) replacesRow(cp checkpoint.Checkpoint) bool {
	return r.opts.ReplaceDone && cp.Phase == checkpoint.PhaseDone
}

// replaceDoneRow deletes the done row cp under the run's own lock session
// (LK-1), so the run can start fresh on the table without a release in
// which another engine could take it. Only a row whose run has nothing
// left on the server is replaced: the source a done row's swap retained,
// kept under KeepOld for a rollback window, is still that run's, and the
// row's proof is the only record of which relation it is — so a done row
// whose retained source still stands is refused as resume-row-terminal,
// naming the relation, until the caller ends the window with
// DropOldTable. Delete removes only the row with the Identity the
// inspection showed (ST-2); a row another engine wrote in the meantime
// survives and comes back as its IncompatibleError.
func (r *Runner) replaceDoneRow(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, cp checkpoint.Checkpoint) error {
	proof, err := rowProof(cp)
	if err != nil {
		return err
	}
	retained, err := retainedSource(ctx, pool, lock, proof, r.opts.Shadow)
	if err != nil {
		return err
	}
	if retained.exists {
		return refuse(CauseResumeTerminal, nil, "checkpoint row for %s.%s is done and the source its swap retained still stands as %s.%s; end the rollback window with DropOldTable before running the table again", cp.Schema, cp.Table, retained.schema, retained.name)
	}
	return r.store.Delete(ctx, lock, cp.Schema, cp.Table, cp.Identity())
}

// retainedRelation is where the catalog shows a done run's retained
// source: the schema and name it bears now, or exists false once it has
// been dropped.
type retainedRelation struct {
	schema, name string
	exists       bool
}

// retainedSource looks up the source relation a done row's proof names,
// by OID, since the name it bears is not the question — a relation under
// the _old name that is not this source is the cutover gate's to refuse,
// and this source renamed elsewhere is still this run's. The read runs in
// its own bounded transaction under the lock session, confirming the lock
// before it reads (LK-1).
func retainedSource(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, proof Proof, opts Options) (retainedRelation, error) {
	ctx, stop := lock.Bind(ctx)
	defer stop()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return retainedRelation{}, fmt.Errorf("begin retained source lookup for %s.%s: %w", proof.Schema, proof.SourceTable, err)
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed; on a failure path the server aborts
		// the transaction with its session either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	if err := setBudgets(ctx, tx, opts); err != nil {
		return retainedRelation{}, err
	}
	if err := confirmTableLock(ctx, tx, lock); err != nil {
		return retainedRelation{}, fmt.Errorf("retained source lookup for %s.%s: %w", proof.Schema, proof.SourceTable, err)
	}
	schema, name, exists, err := lookupRelationName(ctx, tx, proof.SourceOID)
	if err != nil {
		return retainedRelation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return retainedRelation{}, fmt.Errorf("commit retained source lookup for %s.%s: %w", proof.Schema, proof.SourceTable, err)
	}
	return retainedRelation{schema: schema, name: name, exists: exists}, nil
}
