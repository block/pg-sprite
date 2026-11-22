package executor

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/statement"
)

// processChildren gives every direct partition of a table an index attached
// to parentIdx, the table's own index. A partition that already has one
// attached is left alone; a sub-partitioned one is still descended into,
// because its own partitions may not have theirs.
func (f *partitionedIndexFlow) processChildren(ctx context.Context, parentTableOID uint32, parentIdx relationFacts, tree []partitionNode) error {
	for _, node := range childrenOf(tree, parentTableOID) {
		var err error
		switch {
		case node.foreign():
			// Unreachable after readTree's refusal; kept so a node kind the
			// build cannot index is never handed to the leaf path.
			err = fmt.Errorf("%w: %s", ErrForeignTablePartition, node.qualified())
		case node.subPartitioned():
			err = f.processSubPartitioned(ctx, node, parentIdx, tree)
		default:
			err = f.processLeaf(ctx, node, parentIdx)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// processLeaf gives a leaf partition an index attached to parentIdx: the
// index an earlier run built under the derived name, a matching index the
// table already has, or a new concurrent build under the derived name.
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
	step := &partitionStep{flow: f}

	idx, debris, err := f.findLeafIndex(ctx, node, name)
	if err != nil {
		return err
	}
	result := PartitionIndexResult{Schema: node.schema, Table: node.name, Action: PartitionIndexReused}
	if idx.oid != 0 && idx.name != name {
		result.Action = PartitionIndexAdopted
	}
	if idx.oid == 0 {
		result.Action = PartitionIndexBuilt
		if debris {
			step.start(progress.OperationConcurrentIndex, buildSQL)
			// The invalid index under the derived name is the debris of an
			// earlier run's failed build of this partition. The recovery
			// proves it abandoned under the partition's own lock,
			// quarantines it by OID, and drops it; a backend still building
			// it refuses the run.
			if _, err := recoverAbandonedIndex(ctx, f.pool, buildSQL, f.budget, recoveryPool{
				minConns: dropRecoveryMinConns,
				sessions: "recovery session and drop session",
			}, nil); err != nil {
				return fmt.Errorf("recover the invalid index %s on partition %s: %w",
					qualifiedName(node.schema, name), node.qualified(), err)
			}
			f.changes++
			result.RecoveredDebris = true
		}
		step.start(progress.OperationConcurrentIndex, buildSQL)
		built, err := buildIndexConcurrently(ctx, f.pool, buildSQL, f.budget, f.tracker)
		if err != nil {
			return fmt.Errorf("build the index of partition %s: %w", node.qualified(), err)
		}
		f.changes++
		if idx, err = f.readBuiltIndex(ctx, node, name, built.IndexOID); err != nil {
			return err
		}
	}
	if err := f.attach(ctx, node, parentIdx, idx, func(sql string) { step.start(progress.OperationBrief, sql) }); err != nil {
		return err
	}
	result.Index, result.IndexOID = idx.name, idx.oid
	f.rep.Worked = append(f.rep.Worked, result)
	return nil
}

// findLeafIndex looks for an index the leaf partition can be attached with
// before the build creates one: the index under the derived name when an
// earlier run left a usable one, or any matching index the table already
// has. It returns the zero value when the build has to create one, and says
// when the partition holds debris that must be removed first: an invalid
// index under the derived name, or one an earlier recovery quarantined.
func (f *partitionedIndexFlow) findLeafIndex(ctx context.Context, node partitionNode, name string) (idx relationFacts, debris bool, err error) {
	var (
		occupant   relationFacts
		occupied   bool
		reusable   []relationFacts
		quarantine []invalidIndex
	)
	err = f.tx.read(ctx, "inspect the index of partition "+node.qualified(), func(ctx context.Context, q querier) error {
		var err error
		if occupant, occupied, err = relationByName(ctx, q, node.schema, name); err != nil || occupied {
			return err
		}
		if reusable, err = reusableIndexes(ctx, q, node.oid); err != nil {
			return err
		}
		// A recovery that renamed an abandoned index to its quarantine name
		// and stopped before the drop leaves the derived name free and the
		// debris in place; the concurrent build refuses a table that still
		// carries it.
		quarantine, err = listDroppableQuarantinedIndexes(ctx, q, indexTarget{tableOID: node.oid, schema: node.schema, table: node.name})
		return err
	})
	if err != nil {
		return relationFacts{}, false, err
	}
	if occupied {
		state, err := f.judgeOccupant(occupant, node, "i")
		if err != nil {
			return relationFacts{}, false, err
		}
		if state == occupantUsable {
			return occupant, false, nil
		}
		return relationFacts{}, true, nil
	}
	for _, candidate := range reusable {
		key, err := candidate.definitionKey()
		if err != nil {
			return relationFacts{}, false, err
		}
		if key == f.rootKey {
			return candidate, false, nil
		}
	}
	// Nothing to reuse, so the build runs, and it needs the quarantined
	// debris of an earlier recovery gone first.
	return relationFacts{}, len(quarantine) > 0, nil
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

// partitionStep moves the tracker to a new step on a partition's first
// statement and keeps the later statements of the same partition on it.
type partitionStep struct {
	flow    *partitionedIndexFlow
	started bool
}

func (s *partitionStep) start(op progress.Operation, sql string) {
	s.flow.startStep(op, sql, !s.started)
	s.started = true
}

// processSubPartitioned gives a partition that is itself partitioned an
// index of its own, ON ONLY, fills it by recursing into its partitions, and
// attaches it to parentIdx. The order is what makes each level valid: an
// index attached to its parent when the partition's own partitions have
// theirs.
func (f *partitionedIndexFlow) processSubPartitioned(ctx context.Context, node partitionNode, parentIdx relationFacts, tree []partitionNode) error {
	var (
		idx      relationFacts
		attached bool
	)
	err := f.tx.read(ctx, "inspect the index of partition "+node.qualified(), func(ctx context.Context, q querier) error {
		oid, found, err := attachedIndexOf(ctx, q, node.oid, parentIdx.oid)
		if err != nil || !found {
			return err
		}
		attached = true
		idx, found, err = relationByOID(ctx, q, oid)
		if err == nil && !found {
			err = fmt.Errorf("%w: index %d attached under %s is not in the catalog", ErrInvariantViolation, oid, qualifiedName(parentIdx.schema, parentIdx.name))
		}
		return err
	})
	if err != nil {
		return err
	}
	step := &partitionStep{flow: f}
	if !attached {
		if idx, err = f.ensureSubPartitionIndex(ctx, node, step); err != nil {
			return err
		}
	}
	if err := f.processChildren(ctx, node.oid, idx, tree); err != nil {
		return err
	}
	if attached {
		return nil
	}
	// The partition's step began with its own index when the build created
	// it, and the attach, after its partitions' steps, stays on the latest
	// one so steps never go back. A resumed index was not created here, so
	// the attach starts the partition's step.
	if err := f.attach(ctx, node, parentIdx, idx, func(sql string) { f.startStep(progress.OperationBrief, sql, !step.started) }); err != nil {
		return err
	}
	f.rep.Worked = append(f.rep.Worked, PartitionIndexResult{
		Schema: node.schema, Table: node.name, Index: idx.name, IndexOID: idx.oid, Action: PartitionIndexOnly,
	})
	return nil
}

// ensureSubPartitionIndex returns the ON ONLY index of a sub-partitioned
// partition under the derived name, creating it when no relation holds the
// name and resuming it when an earlier run created it.
func (f *partitionedIndexFlow) ensureSubPartitionIndex(ctx context.Context, node partitionNode, step *partitionStep) (relationFacts, error) {
	name := derivedPartitionIndexName(f.req.index, node.oid)
	var (
		occupant relationFacts
		occupied bool
	)
	err := f.tx.read(ctx, "inspect the index of partition "+node.qualified(), func(ctx context.Context, q querier) error {
		var err error
		occupant, occupied, err = relationByName(ctx, q, node.schema, name)
		return err
	})
	if err != nil {
		return relationFacts{}, err
	}
	if occupied {
		// A partitioned index is invalid until its own partitions have
		// theirs, so a valid-or-invalid one of the requested definition is a
		// prefix to resume, never debris.
		if _, err := f.judgeOccupant(occupant, node, "I"); err != nil {
			return relationFacts{}, err
		}
		return occupant, nil
	}
	createSQL, err := statement.PartitionIndex(f.rootDef, node.schema, node.name, name, statement.PartitionIndexOnly)
	if err != nil {
		return relationFacts{}, fmt.Errorf("make the index statement for partition %s: %w", node.qualified(), err)
	}
	step.start(progress.OperationBrief, createSQL)
	var created relationFacts
	err = f.tx.run(ctx, "create the index of partition "+node.qualified(), catalogReadPath(), true,
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			created, err = createOnlyIndex(ctx, tx, onlyIndexTable{oid: node.oid, schema: node.schema, name: node.name}, node.schema, name, createSQL)
			return err
		})
	if err != nil {
		return relationFacts{}, err
	}
	f.changes++
	return created, nil
}

type occupantState int

const (
	// occupantUsable is a valid index of the requested definition on the
	// partition, which the build attaches as it stands.
	occupantUsable occupantState = iota + 1
	// occupantDebris is an invalid index of the requested definition on the
	// partition: the leftover of a build that did not finish.
	occupantDebris
)

// judgeOccupant decides whether the relation holding a derived index name
// is the index the build would create there. wantKind is the relkind that
// index has: 'i' for a leaf partition's, 'I' for a sub-partitioned one's.
// Anything that is not that index, on that partition, with the requested
// definition and attached to nothing, is a name the build cannot use and
// will not take from its owner. An invalid leaf index of the right
// definition is debris, and an invalid partitioned one is a prefix to
// resume: it is invalid until its own partitions have theirs.
func (f *partitionedIndexFlow) judgeOccupant(occ relationFacts, node partitionNode, wantKind string) (occupantState, error) {
	name := qualifiedName(occ.schema, occ.name)
	if occ.relkind != wantKind || occ.tableOID != node.oid {
		return 0, fmt.Errorf("%w: %s is %s, not the index %s needs",
			ErrPartitionedIndexNameTaken, name, describeRelation(occ), node.qualified())
	}
	if occ.isPartition {
		return 0, fmt.Errorf("%w: %s is already attached to another partitioned index",
			ErrPartitionedIndexNameTaken, name)
	}
	// A constraint's index has duties beyond its key, so attaching it under
	// a parent index that is not a constraint's would change what it is.
	if occ.backsConstraint {
		return 0, fmt.Errorf("%w: %s backs a constraint on partition %s",
			ErrPartitionedIndexNameTaken, name, node.qualified())
	}
	key, err := occ.definitionKey()
	if err != nil {
		return 0, err
	}
	if key != f.rootKey {
		return 0, fmt.Errorf("%w: %s on partition %s does not have the requested definition",
			ErrPartitionedIndexNameTaken, name, node.qualified())
	}
	if wantKind == "i" && !occ.valid {
		return occupantDebris, nil
	}
	return occupantUsable, nil
}

// alreadyAttached reports whether the partition already has an index
// attached to parentIdx, which leaves nothing for the build to do on it.
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
// idempotent on the server, so a rerun after an unknown commit outcome is
// safe.
func (f *partitionedIndexFlow) attach(ctx context.Context, node partitionNode, parentIdx, child relationFacts, announce func(sql string)) error {
	attachSQL := "ALTER INDEX " + pgx.Identifier{parentIdx.schema, parentIdx.name}.Sanitize() +
		" ATTACH PARTITION " + pgx.Identifier{child.schema, child.name}.Sanitize()
	announce(attachSQL)
	err := f.tx.run(ctx, "attach the index of partition "+node.qualified(), catalogReadPath(), true,
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, attachSQL)
			return err
		})
	if err != nil {
		return err
	}
	f.changes++
	return nil
}
