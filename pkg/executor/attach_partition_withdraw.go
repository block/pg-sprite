package executor

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/progress"
)

// What an online attach that stops before its ATTACH commits does with the
// DEFAULT partition's proof. That CHECK refuses every write the parent
// routes to the DEFAULT partition inside the new bound, a window the caller
// accepted only for the run that attaches, so a run that does not attach
// takes it off again. The CHECKs on the table being attached stay: each
// refuses only rows the attach would refuse, and the rerun finds them.

// attachDefaultProofWithdrawn says what the withdrawal did.
type attachDefaultProofWithdrawn int

const (
	// attachDefaultProofAbsent: the DEFAULT partition carries no proof.
	attachDefaultProofAbsent attachDefaultProofWithdrawn = iota + 1
	// attachDefaultProofKept: the table is a partition of the parent, so
	// the ATTACH committed, and its own transaction dropped the proof.
	attachDefaultProofKept
	// attachDefaultProofDropped: the table is not attached, and the
	// withdrawal dropped the proof.
	attachDefaultProofDropped
)

// withdrawDefaultProof is called with the error that stopped a run whose
// parent has a DEFAULT partition, and drops that partition's proof when the
// table is not attached. It returns the run's error, saying what it did
// with the proof, and, when the proof could not be dropped, the statement
// that drops it.
//
// It runs under a context that outlives the run's: a caller's cancel, an
// expired budget, or a lost table lock is the stop it cleans up after. Its
// own bound is the holder wait plus sessionCleanupTimeout, and every
// statement runs under the pool's lock_timeout and statement_timeout.
// Without the table lock it is still safe: the ATTACH holds ACCESS
// EXCLUSIVE on the DEFAULT partition until it ends and drops the proof in
// its own transaction, so the withdrawal, which locks the DEFAULT partition
// before it reads, sees the attach either committed, and keeps out, or
// rolled back.
func (f attachPartitionFlow) withdrawDefaultProof(ctx context.Context, target attachPartitionTarget, cause error) error {
	proof := target.deflt
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), f.opts.HolderWait+sessionCleanupTimeout)
	defer cancel()
	withdrawn, err := dropDefaultProofUnlessAttached(cleanupCtx, f, target)
	if err != nil {
		return fmt.Errorf("%w; the DEFAULT partition %s may still carry the CHECK %s, which refuses the writes the parent routes there inside the new bound, and taking it off failed: if %s is not a partition of %s, run %s (withdraw: %w)",
			cause, proof.qualified(), proof.name(), target.bound.qualified(),
			qualifiedName(f.attach.ParentSchema, f.attach.Parent), proof.dropSQL(), err)
	}
	switch withdrawn {
	case attachDefaultProofDropped:
		f.record(AttachPartitionStepDefaultProof, proof.name(), attachPartitionStepDropped)
		return fmt.Errorf("%s was not attached, so the CHECK %s was dropped from the DEFAULT partition %s, whose writes inside the new bound go on; rerunning the same statement resumes: %w",
			target.bound.qualified(), proof.name(), proof.qualified(), cause)
	case attachDefaultProofAbsent, attachDefaultProofKept:
		return cause
	default:
		return fmt.Errorf("%w: unknown withdrawal outcome %d of attach proof %s; %w", ErrInvariantViolation, withdrawn, proof.name(), cause)
	}
}

// dropDefaultProofUnlessAttached waits outside the lock queue, up to the
// holder wait, then in one brief transaction locks the DEFAULT partition,
// proves it, reads whether the table is a partition of the parent, and
// drops the proof only when it is not and the proof is there.
func dropDefaultProofUnlessAttached(ctx context.Context, f attachPartitionFlow, target attachPartitionTarget) (attachDefaultProofWithdrawn, error) {
	proof := target.deflt
	// INV: LK-2 — the DROP takes ACCESS EXCLUSIVE, so it requests its lock
	// once no long-lived reader or writer holds the DEFAULT partition, or
	// once the holder wait runs out.
	if _, err := waitOutsideLockQueue(ctx, f.pool, proof.lockedTables(), f.opts.HolderWait, systemHolderWaitClock(), trackerHolderWaitReport(f.tracker)); err != nil {
		return 0, err
	}
	var withdrawn attachDefaultProofWithdrawn
	err := briefTx{pool: f.pool}.run(ctx, "withdraw the attach proof of "+proof.qualified(), catalogReadPath(), true,
		func(ctx context.Context, tx pgx.Tx) error {
			// INV: ST-7 — the DEFAULT partition stays under its name, and
			// an ATTACH that holds it has ended, until the transaction ends.
			if err := lockAndProveAttachProofTable(ctx, tx, proof); err != nil {
				return err
			}
			m, err := readAttachMembership(ctx, tx, target.bound)
			if err != nil {
				return err
			}
			if m.isPartitionOf(target.parentOID) {
				withdrawn = attachDefaultProofKept
				return nil
			}
			state, _, err := readAttachProof(ctx, tx, proof)
			if err != nil {
				return err
			}
			if state == attachProofAddNeeded {
				withdrawn = attachDefaultProofAbsent
				return nil
			}
			f.started(progress.OperationBrief, proof.dropSQL())
			// INV: LK-2 — dropping a CHECK changes only the catalog, under
			// the pool's lock_timeout and statement_timeout.
			if _, err := tx.Exec(ctx, proof.dropSQL()); err != nil {
				return err
			}
			withdrawn = attachDefaultProofDropped
			return nil
		})
	if err != nil {
		return 0, err
	}
	return withdrawn, nil
}
