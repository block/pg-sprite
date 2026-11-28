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
// takes it off again, and so does a rerun refused before it adds anything
// that finds one an earlier run left. The CHECKs on the table being
// attached stay: each refuses only rows the attach would refuse, and the
// rerun finds them.
//
// A process that dies between the proof's ADD and the ATTACH runs none of
// this, and neither does a run that lost track of the ADD's COMMIT while it
// was landing: the proof stays until the same statement runs again.

// attachDefaultProofWithdrawn says what the withdrawal did.
type attachDefaultProofWithdrawn int

const (
	// attachDefaultProofAbsent: the DEFAULT partition carries no proof.
	attachDefaultProofAbsent attachDefaultProofWithdrawn = iota + 1
	// attachDefaultProofKept: the table is a partition of the parent, so
	// the ATTACH committed, and its own transaction dropped the proof, or
	// someone else attached it, which makes the proof redundant.
	attachDefaultProofKept
	// attachDefaultProofDropped: the table is not attached, and the
	// withdrawal dropped the proof.
	attachDefaultProofDropped
)

// AttachDefaultProofLeftError is returned when an online attach stopped
// without attaching and could not take the DEFAULT partition's proof off
// again. Until it is dropped, the CHECK refuses the writes the parent
// routes to the DEFAULT partition inside the new bound. Rerunning the same
// statement drops it, or attaches the table; an operator who will not rerun
// it runs DropSQL, but only while Table is not a partition of Parent.
type AttachDefaultProofLeftError struct {
	// DefaultPartition is the DEFAULT partition, schema-qualified.
	DefaultPartition string
	// Constraint is the proof CHECK's name.
	Constraint string
	// DropSQL is the ALTER TABLE ... DROP CONSTRAINT that drops it.
	DropSQL string
	// Table is the table the statement attaches, and Parent the
	// partitioned table, schema-qualified.
	Table  string
	Parent string
	// Seen is true when the withdrawal read the CHECK on the DEFAULT
	// partition before the drop failed, and false when that read failed, so
	// the CHECK may or may not be there.
	Seen bool
	// Cause is the error that stopped the run, and Err why the CHECK was not
	// dropped.
	Cause error
	Err   error
}

func (e *AttachDefaultProofLeftError) Error() string {
	carries := "carries"
	if !e.Seen {
		carries = "may carry"
	}
	return fmt.Sprintf("%v; the DEFAULT partition %s %s the CHECK %s, which refuses the writes the parent routes there inside the new bound, and taking it off failed: if %s is not a partition of %s, run %s (withdraw: %v)",
		e.Cause, e.DefaultPartition, carries, e.Constraint, e.Table, e.Parent, e.DropSQL, e.Err)
}

// Unwrap returns the error that stopped the run and the withdrawal's.
func (e *AttachDefaultProofLeftError) Unwrap() []error { return []error{e.Cause, e.Err} }

// withdrawLeftoverDefaultProof is called with the refusal resolve returned
// once it had read the parent's DEFAULT partition, and drops a proof an
// earlier run left there, as withdrawDefaultProof does. The table may not
// resolve at all; then nothing is attached under the statement's name.
func (f attachPartitionFlow) withdrawLeftoverDefaultProof(ctx context.Context, r attachResolved, cause error) error {
	cleanupCtx, cancel := f.withdrawContext(ctx)
	defer cancel()
	_, exclusion, err := deriveAttachBoundCheck(cleanupCtx, briefTx{pool: f.pool}, r.parentOID, f.attach.Bound)
	if err != nil {
		return fmt.Errorf("%w; whether the DEFAULT partition %s carries a proof an earlier run left was not read: %w",
			cause, qualifiedName(r.deflt.schema, r.deflt.name), err)
	}
	target := attachPartitionTarget{parentOID: r.parentOID, deflt: r.defaultProof(exclusion)}
	if r.table.oid != 0 {
		target.bound = attachProof{kind: attachProofBound, tableOID: r.table.oid, schema: r.table.schema, table: r.table.name}
	}
	return f.withdrawDefaultProof(ctx, target, cause)
}

// withdrawContext is the context the withdrawal runs under. It outlives the
// run's: a caller's cancel, an expired budget, or a lost table lock is the
// stop it cleans up after. Its own bound is the holder wait plus
// sessionCleanupTimeout.
func (f attachPartitionFlow) withdrawContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), f.opts.HolderWait+sessionCleanupTimeout)
}

// withdrawDefaultProof is called with the error that stopped a run whose
// parent has a DEFAULT partition, and drops that partition's proof when the
// table is not attached. It returns the run's error, saying what it did
// with the proof, or an *AttachDefaultProofLeftError when the proof could
// not be dropped. A target whose table did not resolve has no bound proof.
//
// It runs under withdrawContext, and every statement runs under the pool's
// lock_timeout and statement_timeout. Without the table lock it is still
// safe: the ATTACH holds ACCESS EXCLUSIVE on the DEFAULT partition until it
// ends and drops the proof in its own transaction, so the withdrawal, which
// locks the DEFAULT partition before it decides, sees the attach either
// committed, and keeps out, or rolled back.
func (f attachPartitionFlow) withdrawDefaultProof(ctx context.Context, target attachPartitionTarget, cause error) error {
	proof := target.deflt
	cleanupCtx, cancel := f.withdrawContext(ctx)
	defer cancel()
	withdrawn, seen, err := dropDefaultProofUnlessAttached(cleanupCtx, f, target)
	if err != nil {
		return &AttachDefaultProofLeftError{
			DefaultPartition: proof.qualified(),
			Constraint:       proof.name(),
			DropSQL:          proof.dropSQL(),
			Table:            f.attachedTableName(target),
			Parent:           qualifiedName(f.attach.ParentSchema, f.attach.Parent),
			Seen:             seen,
			Cause:            cause,
			Err:              err,
		}
	}
	switch withdrawn {
	case attachDefaultProofDropped:
		f.record(AttachPartitionStepDefaultProof, proof.name(), attachPartitionStepDropped)
		return fmt.Errorf("%s is not attached, so the CHECK %s was dropped from the DEFAULT partition %s, whose writes inside the new bound go on: %w",
			f.attachedTableName(target), proof.name(), proof.qualified(), cause)
	case attachDefaultProofAbsent, attachDefaultProofKept:
		return cause
	default:
		return fmt.Errorf("%w: unknown withdrawal outcome %d of attach proof %s; %w", ErrInvariantViolation, withdrawn, proof.name(), cause)
	}
}

// attachedTableName is the table the statement attaches: as the run
// resolved it, or as the statement spells it when it did not resolve.
func (f attachPartitionFlow) attachedTableName(target attachPartitionTarget) string {
	if target.bound.tableOID == 0 {
		return qualifiedName(f.attach.Schema, f.attach.Name)
	}
	return target.bound.qualified()
}

// dropDefaultProofUnlessAttached first reads, without a lock, whether the
// DEFAULT partition carries the proof, and returns at once when it does
// not, so a run refused before it added anything neither waits for nor
// locks the DEFAULT partition. Otherwise it waits outside the lock queue,
// up to the holder wait, then in one brief transaction locks the DEFAULT
// partition, proves it, reads whether the table is a partition of the
// parent, and drops the proof only when it is not and the proof is still
// there. seen says whether the first read found the proof.
func dropDefaultProofUnlessAttached(ctx context.Context, f attachPartitionFlow, target attachPartitionTarget) (withdrawn attachDefaultProofWithdrawn, seen bool, err error) {
	proof := target.deflt
	var present attachProofState
	err = briefTx{pool: f.pool}.read(ctx, "look for the attach proof of "+proof.qualified(), func(ctx context.Context, q querier) error {
		var err error
		present, _, err = readAttachProof(ctx, q, proof)
		return err
	})
	if err != nil {
		return 0, false, err
	}
	// An ATTACH still deciding has not dropped the proof yet, so it is
	// read as present and decided below under the lock.
	if present == attachProofAddNeeded {
		return attachDefaultProofAbsent, false, nil
	}
	// INV: LK-2 — the DROP takes ACCESS EXCLUSIVE, so it requests its lock
	// once no long-lived reader or writer holds the DEFAULT partition, or
	// once the holder wait runs out.
	if _, err := waitOutsideLockQueue(ctx, f.pool, proof.lockedTables(), f.opts.HolderWait, systemHolderWaitClock(), trackerHolderWaitReport(f.tracker)); err != nil {
		return 0, true, err
	}
	err = briefTx{pool: f.pool}.run(ctx, "withdraw the attach proof of "+proof.qualified(), catalogReadPath(), true,
		func(ctx context.Context, tx pgx.Tx) error {
			// INV: ST-7 — the DEFAULT partition stays under its name, and
			// an ATTACH that holds it has ended, until the transaction ends.
			if err := lockAndProveAttachProofTable(ctx, tx, proof); err != nil {
				return err
			}
			attached, err := attachTableIsPartitionOf(ctx, tx, target)
			if err != nil {
				return err
			}
			if attached {
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
		return 0, true, err
	}
	return withdrawn, true, nil
}

// attachTableIsPartitionOf says whether the table the target attaches is a
// partition of the parent. A table that did not resolve is not one under
// the statement's name.
func attachTableIsPartitionOf(ctx context.Context, q querier, target attachPartitionTarget) (bool, error) {
	if target.bound.tableOID == 0 {
		return false, nil
	}
	m, err := readAttachMembership(ctx, q, target.bound)
	if err != nil {
		return false, err
	}
	return m.isPartitionOf(target.parentOID), nil
}
