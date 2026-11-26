package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/progress"
)

// partitionedKeyFlow is one run of the partitioned key build under the
// parent's table lock. The parent's step is in partitioned_key_parent.go.
type partitionedKeyFlow struct {
	tx      briefTx
	pool    *pgxpool.Pool
	budget  ConcurrentBudget
	tracker *progress.Tracker
	// runStep is the step of the run that owns the tracker; every report
	// goes to it.
	runStep int
	req     partitionedKeyRequest
	target  indexTarget

	// leaves is each leaf's key as its latest leaf step left it, by the
	// leaf's OID: the constraint and index the parent's step proves are still
	// there before it adds the parent's constraint.
	leaves map[uint32]leafKeyResult
	rep    PartitionedKeyReport
}

// runPartitionedKey is the build's body, run under the context the table
// lock is bound to.
func runPartitionedKey(ctx context.Context, pool *pgxpool.Pool, req partitionedKeyRequest, b ConcurrentBudget, tracker *progress.Tracker, runStep int) (PartitionedKeyReport, error) {
	f := &partitionedKeyFlow{
		tx: briefTx{pool: pool}, pool: pool, budget: b, tracker: tracker, runStep: runStep, req: req,
		leaves: map[uint32]leafKeyResult{},
		rep:    PartitionedKeyReport{Schema: req.schema, Table: req.table, Constraint: req.spec.name},
	}
	err := f.run(ctx)
	return f.rep, err
}

func (f *partitionedKeyFlow) run(ctx context.Context) error {
	if err := f.resolveTarget(ctx); err != nil {
		return err
	}
	done, err := f.checkParent(ctx)
	if err != nil {
		return err
	}
	if done {
		return nil
	}
	tree, err := f.readTree(ctx)
	if err != nil {
		return err
	}
	passes, err := settlePasses(ctx, maxPartitionedKeyPasses, ErrPartitionedKeyUnsettled, func(ctx context.Context, pass int) (bool, error) {
		if pass > 1 {
			var err error
			if tree, err = f.readTree(ctx); err != nil {
				return false, err
			}
		}
		if err := f.ensureLeaves(ctx, tree); err != nil {
			return false, err
		}
		added, err := f.addParentKey(ctx)
		// A pass that could not add the parent's constraint found a
		// partition it had not covered, and the next pass covers it.
		return !added, err
	})
	f.rep.Passes = passes
	if errors.Is(err, ErrPartitionedKeyUnsettled) {
		return fmt.Errorf("%w: %s still had partitions without the key constraint after %d passes",
			err, qualifiedName(f.req.schema, f.req.table), maxPartitionedKeyPasses)
	}
	return err
}

// resolveTarget re-proves, under the lock, what the preflight proved before
// it: the table the statement names exists and is partitioned. In the same
// read it refuses a key that lacks a partition key column of a partitioned
// table in the tree, the check the partitioned unique index build makes, with
// its error.
func (f *partitionedKeyFlow) resolveTarget(ctx context.Context) error {
	return f.tx.read(ctx, "resolve the partitioned table and its partition keys", func(ctx context.Context, q querier) error {
		target, err := resolveTarget(ctx, q, concurrentIndexBuild{tableSchema: f.req.schema, table: f.req.table})
		if err != nil {
			return err
		}
		var relkind string
		if err := q.QueryRow(ctx, relkindSQL, target.tableOID).Scan(&relkind); err != nil {
			return fmt.Errorf("read the kind of table %s: %w", qualifiedName(target.schema, target.table), err)
		}
		if relkind != "p" {
			return fmt.Errorf("%w: %s is not a partitioned table when the catalog is read under the table lock",
				ErrNotPartitionedTable, qualifiedName(target.schema, target.table))
		}
		tree, err := listPartitionTree(ctx, q, target.tableOID)
		if err != nil {
			return err
		}
		if err := readUncoveredPartitionKeys(ctx, q, target.schema, f.req.spec.name, f.req.spec.columns, keyedTables(target, tree)); err != nil {
			return err
		}
		f.target = target
		return nil
	})
}

// parentNodeOf is the partitioned table itself in the shape the column read
// takes.
func parentNodeOf(target indexTarget) partitionNode {
	return partitionNode{oid: target.tableOID, schema: target.schema, name: target.table, relkind: "p"}
}

// checkParent reads what the parent already has and decides whether the
// build may start (false), the parent already carries exactly the requested
// constraint and every partition's is adopted, which the build reports as
// done (true), or something stands in the way (a typed refusal). It runs
// before any partition is touched, so a refusal leaves the tree as it was,
// and a run resumed after the parent's constraint was added does not mistake
// the adopted partitions' constraints for conflicts.
func (f *partitionedKeyFlow) checkParent(ctx context.Context) (bool, error) {
	var done bool
	err := f.tx.read(ctx, "inspect the constraints of "+qualifiedName(f.target.schema, f.target.table), func(ctx context.Context, q querier) error {
		columns, err := readLeafKeyColumns(ctx, q, parentNodeOf(f.target), f.req.spec.columns)
		if err != nil {
			return err
		}
		constraints, err := readParentConstraints(ctx, q, f.target.tableOID)
		if err != nil {
			return err
		}
		holder, held, err := relationByName(ctx, q, f.target.schema, f.req.spec.name)
		if err != nil {
			return err
		}
		existing, found, err := decideParentKey(f.req.spec, parentNodeOf(f.target), columns, constraints, holder, held)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		partitions, err := requireAdoptedEverywhere(ctx, q, parentNodeOf(f.target), existing)
		if err != nil {
			return err
		}
		if err := q.QueryRow(ctx, `SELECT pg_catalog.current_setting('server_version')`).Scan(&f.rep.ServerVersion); err != nil {
			return fmt.Errorf("read server_version: %w", err)
		}
		done = true
		f.rep.ConstraintOID, f.rep.Partitions = existing.oid, partitions
		return nil
	})
	return done, err
}

// readTree lists the partition tree and refuses a tree the build does not
// take. It is read at the start and on every later pass, so a foreign table
// or a sub-partitioned partition attached mid-run refuses the run too.
func (f *partitionedKeyFlow) readTree(ctx context.Context) ([]partitionNode, error) {
	var tree []partitionNode
	err := f.tx.read(ctx, "list the partition tree", func(ctx context.Context, q querier) error {
		var err error
		tree, err = listPartitionTree(ctx, q, f.target.tableOID)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := refuseKeyTree(f.target, tree); err != nil {
		return nil, err
	}
	f.rep.Partitions = len(tree)
	return tree, nil
}

// refuseKeyTree refuses a tree holding a foreign table, which cannot carry
// the index a key needs, or a sub-partitioned partition, which the build does
// not take.
func refuseKeyTree(target indexTarget, tree []partitionNode) error {
	parent := qualifiedName(target.schema, target.table)
	if foreign := foreignPartitions(tree); len(foreign) > 0 {
		return fmt.Errorf("%w: %s under %s", ErrForeignTablePartition, strings.Join(foreign, ", "), parent)
	}
	var nested []string
	for _, n := range tree {
		if n.subPartitioned() {
			nested = append(nested, n.qualified())
		}
	}
	if len(nested) > 0 {
		return fmt.Errorf("%w: %s under %s", ErrPartitionedKeySubPartitioned, strings.Join(nested, ", "), parent)
	}
	return nil
}

// ensureLeaves gives every leaf in the tree the key constraint, in order,
// and records what each step left. A leaf's first result is the one the
// report keeps: a later pass finds what the first one made.
func (f *partitionedKeyFlow) ensureLeaves(ctx context.Context, tree []partitionNode) error {
	for _, n := range tree {
		buildSQL := leafKeyIndexSQL(f.req.spec, n, derivedPartitionIndexName(f.req.spec.name, n.oid))
		f.startStep(progress.OperationConcurrentIndex, buildSQL)
		res, err := ensureLeafKey(ctx, f.pool, f.req.spec, n, f.budget)
		if err != nil {
			return err
		}
		if _, seen := f.leaves[n.oid]; !seen {
			action, err := res.action.reported()
			if err != nil {
				return err
			}
			f.rep.Leaves = append(f.rep.Leaves, PartitionKeyResult{
				Schema: n.schema, Table: n.name,
				Constraint: res.constraint, ConstraintOID: res.constraintOID,
				Index: res.index, IndexOID: res.indexOID,
				Action: action, RecoveredDebris: res.recoveredDebris,
			})
		}
		f.leaves[n.oid] = res
	}
	return nil
}

// reported is the leaf step's action as the report names it.
func (a leafKeyAction) reported() (PartitionKeyAction, error) {
	switch a {
	case leafKeyFound:
		return PartitionKeyFound, nil
	case leafKeyConstrained:
		return PartitionKeyConstrained, nil
	case leafKeyBuilt:
		return PartitionKeyBuilt, nil
	default:
		return "", fmt.Errorf("%w: unknown leaf key action %q", ErrInvariantViolation, a)
	}
}

// startStep reports the statement in flight on the run's step.
func (f *partitionedKeyFlow) startStep(op progress.Operation, sql string) {
	if f.tracker == nil {
		return
	}
	f.tracker.StartStep(f.runStep, op, sql)
}
