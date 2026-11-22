package executor

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/statement"
)

// processChildren gives every partition of the table an index attached to
// parentIdx, the table's own index.
func (f *partitionedIndexFlow) processChildren(ctx context.Context, parentTableOID uint32, parentIdx relationFacts, tree []partitionNode) error {
	for _, node := range childrenOf(tree, parentTableOID) {
		if node.subPartitioned() {
			return fmt.Errorf("%w: partition %s is itself partitioned, which this build does not handle yet",
				ErrInvariantViolation, node.qualified())
		}
		if err := f.processLeaf(ctx, node, parentIdx); err != nil {
			return err
		}
	}
	return nil
}

// processLeaf gives a leaf partition an index attached to parentIdx: a new
// concurrent build under the derived name, then the attach.
func (f *partitionedIndexFlow) processLeaf(ctx context.Context, node partitionNode, parentIdx relationFacts) error {
	attached, err := f.alreadyAttached(ctx, node, parentIdx)
	if err != nil || attached {
		return err
	}
	name := derivedPartitionIndexName(f.req.index, node.oid)
	buildSQL, err := statement.PartitionIndex(f.rootDef, node.schema, node.name, name, statement.PartitionIndexConcurrently)
	if err != nil {
		return fmt.Errorf("make the index statement for partition %s: %w", node.qualified(), err)
	}
	if err := f.requireNameFree(ctx, node, name); err != nil {
		return err
	}
	f.startStep(progress.OperationConcurrentIndex, buildSQL, true)
	built, err := buildIndexConcurrently(ctx, f.pool, buildSQL, f.budget, f.tracker)
	if err != nil {
		return fmt.Errorf("build the index of partition %s: %w", node.qualified(), err)
	}
	idx, err := f.readBuiltIndex(ctx, node, name, built.IndexOID)
	if err != nil {
		return err
	}
	if err := f.attach(ctx, node, parentIdx, idx); err != nil {
		return err
	}
	f.rep.Worked = append(f.rep.Worked, PartitionIndexResult{
		Schema: node.schema, Table: node.name, Index: idx.name, IndexOID: idx.oid, Action: PartitionIndexBuilt,
	})
	return nil
}

// requireNameFree refuses a partition whose derived index name is held by any
// relation: the build creates the index under that name and takes nothing it
// did not create.
func (f *partitionedIndexFlow) requireNameFree(ctx context.Context, node partitionNode, name string) error {
	return f.tx.read(ctx, "inspect the index of partition "+node.qualified(), func(ctx context.Context, q querier) error {
		occupant, taken, err := relationByName(ctx, q, node.schema, name)
		if err != nil {
			return err
		}
		if taken {
			return fmt.Errorf("%w: %s is %s, not an index the build created on %s",
				ErrPartitionedIndexNameTaken, qualifiedName(occupant.schema, occupant.name), describeRelation(occupant), node.qualified())
		}
		return nil
	})
}

// readBuiltIndex reads the index a build just verified valid, by identity.
func (f *partitionedIndexFlow) readBuiltIndex(ctx context.Context, node partitionNode, name string, oid uint32) (relationFacts, error) {
	var idx relationFacts
	err := f.tx.read(ctx, "read the index built on partition "+node.qualified(), func(ctx context.Context, q querier) error {
		var found bool
		var err error
		idx, found, err = relationByOID(ctx, q, oid)
		if err == nil && !found {
			err = fmt.Errorf("%w: index %s built on partition %s is not in the catalog",
				ErrInvariantViolation, qualifiedName(node.schema, name), node.qualified())
		}
		return err
	})
	return idx, err
}

// alreadyAttached reports whether the partition already has an index attached
// to parentIdx, which leaves nothing for the build to do on it.
func (f *partitionedIndexFlow) alreadyAttached(ctx context.Context, node partitionNode, parentIdx relationFacts) (bool, error) {
	var found bool
	err := f.tx.read(ctx, "inspect the index of partition "+node.qualified(), func(ctx context.Context, q querier) error {
		oid, ok, err := attachedIndexOf(ctx, q, node.oid, parentIdx.oid)
		if err != nil || !ok {
			return err
		}
		attached, ok, err := relationByOID(ctx, q, oid)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: index %d attached under %s is not in the catalog",
				ErrInvariantViolation, oid, qualifiedName(parentIdx.schema, parentIdx.name))
		}
		// An attached index the server holds invalid keeps the parent's
		// index invalid, and nothing here may build over or drop it: it
		// belongs to the parent's index now. Say which one it is.
		if !attached.valid {
			return fmt.Errorf("%w: partition %s has index %s attached to %s and that index is invalid, so the parent's index cannot become valid until an operator repairs or replaces it",
				ErrInvariantViolation, node.qualified(), qualifiedName(attached.schema, attached.name),
				qualifiedName(parentIdx.schema, parentIdx.name))
		}
		found = true
		return nil
	})
	return found, err
}

// attach attaches child, an index on node, to parentIdx. The statement is
// idempotent on the server, so a rerun after an unknown commit outcome is safe.
func (f *partitionedIndexFlow) attach(ctx context.Context, node partitionNode, parentIdx, child relationFacts) error {
	attachSQL := "ALTER INDEX " + pgx.Identifier{parentIdx.schema, parentIdx.name}.Sanitize() +
		" ATTACH PARTITION " + pgx.Identifier{child.schema, child.name}.Sanitize()
	f.startStep(progress.OperationBrief, attachSQL, false)
	return f.tx.run(ctx, "attach the index of partition "+node.qualified(), catalogReadPath(), true,
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, attachSQL)
			return err
		})
}
