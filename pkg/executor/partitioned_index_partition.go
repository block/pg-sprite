package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

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
		// The tree was listed when the pass began. A partition renamed,
		// detached, or replaced since then must not be indexed under the
		// name it used to have (INV: ST-7), and the build below asserts the
		// same identity on its own session.
		if err := f.proveNode(ctx, node); err != nil {
			return err
		}
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
		built, err := buildIndexConcurrentlyOn(ctx, f.pool, buildSQL, f.budget, f.tracker, node.oid)
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
		if occupant, occupied, err = relationByName(ctx, q, node.schema, name); err != nil {
			return err
		}
		if occupied {
			// The derived name decides; the occupant is judged below.
			return nil
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

// readBuiltIndex reads the index a build just verified valid, by identity,
// and refuses it unless it is on the partition the tree listed. The build
// names its table, so a partition dropped and created again under the same
// name while the build ran leaves the index on a table the walk never read.
func (f *partitionedIndexFlow) readBuiltIndex(ctx context.Context, node partitionNode, name string, oid uint32) (relationFacts, error) {
	var idx relationFacts
	err := f.tx.read(ctx, "read the index built on partition "+node.qualified(), func(ctx context.Context, q querier) error {
		var found bool
		var err error
		idx, found, err = relationByOID(ctx, q, oid)
		switch {
		case err != nil:
			return err
		case !found:
			return fmt.Errorf("%w: index %s built on partition %s is not in the catalog",
				ErrInvariantViolation, qualifiedName(node.schema, name), node.qualified())
		case idx.tableOID != node.oid:
			return fmt.Errorf("%w: index %s was built on table oid %d, not on partition %s (oid %d) that the build listed, "+
				"so the partition was replaced while the build ran", ErrInvariantViolation,
				qualifiedName(node.schema, name), idx.tableOID, node.qualified(), node.oid)
		}
		return nil
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
			// The partition is locked and proved by identity before the
			// statement names it, and the index is checked to be on it
			// before the step commits (INV: ST-7).
			// createOnlyIndex locks and proves the partition again. This
			// first lock is what keeps proveNodeIn's read of the tree under
			// the lock; taking a lock the transaction already holds again
			// costs one catalog check.
			if err := lockAndProveTable(ctx, tx, onlyIndexTable{oid: node.oid, schema: node.schema, name: node.name}); err != nil {
				return err
			}
			if err := proveNodeIn(ctx, tx, node); err != nil {
				return err
			}
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

// proveNode checks that the partition the build listed is still what the
// listing said: the same relation, under the same schema and name, and still
// a direct partition of the table it was listed under (INV: ST-7).
func (f *partitionedIndexFlow) proveNode(ctx context.Context, node partitionNode) error {
	return f.tx.read(ctx, "prove partition "+node.qualified()+" is still in the tree", func(ctx context.Context, q querier) error {
		return proveNodeIn(ctx, q, node)
	})
}

const proveNodeSQL = `SELECT EXISTS (
		   SELECT 1
		     FROM pg_catalog.pg_class c
		     JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		     JOIN pg_catalog.pg_inherits h ON h.inhrelid OPERATOR(pg_catalog.=) c.oid
		    WHERE c.oid OPERATOR(pg_catalog.=) $1
		      AND n.nspname OPERATOR(pg_catalog.=) $2
		      AND c.relname OPERATOR(pg_catalog.=) $3
		      AND h.inhparent OPERATOR(pg_catalog.=) $4)`

func proveNodeIn(ctx context.Context, q querier, node partitionNode) error {
	present, err := nodeInTree(ctx, q, node)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("%w: ST-7: partition %s (table %d) is no longer a partition under that name; the partition tree changed since it was listed",
			ErrInvariantViolation, node.qualified(), node.oid)
	}
	return nil
}

// nodeInTree reports whether the partition is still the relation, under the
// schema and name, and the direct partition of the parent, that it was
// listed as.
func nodeInTree(ctx context.Context, q querier, node partitionNode) (bool, error) {
	var present bool
	if err := q.QueryRow(ctx, proveNodeSQL, node.oid, node.schema, node.name, node.parentOID).Scan(&present); err != nil {
		return false, fmt.Errorf("prove partition %s: %w", node.qualified(), err)
	}
	return present, nil
}

// lockAndProveNode takes a bounded lock on a partition and proves, under it,
// that the partition is still in the tree. A detach needs an ACCESS EXCLUSIVE
// lock on the partition, which this lock excludes until the transaction ends,
// so a partition proved here cannot leave the tree before the statement that
// follows. The lock is the weakest one that excludes a detach, and it does not
// block the partition's writers. It is taken under the transaction's own
// lock_timeout (LK-2).
// INV: ST-7 — the statement runs against the partition that was proven.
func lockAndProveNode(ctx context.Context, tx pgx.Tx, node partitionNode) error {
	_, err := tx.Exec(ctx, "LOCK TABLE ONLY "+pgx.Identifier{node.schema, node.name}.Sanitize()+" IN SHARE UPDATE EXCLUSIVE MODE")
	if err != nil {
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == sqlstateUndefinedTable {
			return fmt.Errorf("%w: ST-7: partition %s no longer exists under that name; it was renamed or dropped since the build listed it",
				ErrInvariantViolation, node.qualified())
		}
		return err
	}
	return proveNodeIn(ctx, tx, node)
}

// attach attaches child, an index on node, to parentIdx. Both are named in
// the statement, and PostgreSQL resolves a name, not an identity, so the
// transaction first proves under its own snapshot that each name still
// carries the identity the build resolved (ST-7); an index renamed, dropped,
// or replaced since is refused. The partition is proved to be in the tree
// still, under a lock that keeps it there until the transaction ends; one
// detached since its index was built is refused, and the error names the
// index the build left on it. The statement is idempotent on the server, so
// a rerun after an unknown commit outcome is safe.
func (f *partitionedIndexFlow) attach(ctx context.Context, node partitionNode, parentIdx, child relationFacts, announce func(sql string)) error {
	attachSQL := "ALTER INDEX " + pgx.Identifier{parentIdx.schema, parentIdx.name}.Sanitize() +
		" ATTACH PARTITION " + pgx.Identifier{child.schema, child.name}.Sanitize()
	announce(attachSQL)
	err := f.tx.run(ctx, "attach the index of partition "+node.qualified(), catalogReadPath(), true,
		func(ctx context.Context, tx pgx.Tx) error {
			if err := lockAndProveNode(ctx, tx, node); err != nil {
				if errors.Is(err, ErrInvariantViolation) {
					return f.detachedBeforeAttach(node, child, err)
				}
				return err
			}
			if err := f.proveIdentity(ctx, tx, parentIdx, parentIdx.tableOID); err != nil {
				return err
			}
			if err := f.proveIdentity(ctx, tx, child, node.oid); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, attachSQL); err != nil {
				return err
			}
			// The statement names its indexes and the server resolves names
			// at execution, so what it attached is read back by identity
			// before the transaction commits; any other outcome rolls it
			// back (INV: ST-7).
			var attached bool
			if err := tx.QueryRow(ctx, attachReadBackSQL, child.oid, parentIdx.oid).Scan(&attached); err != nil {
				return fmt.Errorf("read back the attach of %s: %w", qualifiedName(child.schema, child.name), err)
			}
			if !attached {
				return fmt.Errorf("%w: ST-7: after the attach, index %s is not attached to %s; a name resolved to another index",
					ErrInvariantViolation, qualifiedName(child.schema, child.name), qualifiedName(parentIdx.schema, parentIdx.name))
			}
			// The pair can already be attached by another session, so the
			// readback alone does not prove which indexes the names resolved
			// to. The statement now holds locks on what it attached, and each
			// name must still resolve to the identity proven above.
			if err := proveNameBinding(ctx, tx, parentIdx); err != nil {
				return err
			}
			return proveNameBinding(ctx, tx, child)
		})
	if err != nil {
		return err
	}
	f.changes++
	return nil
}

// detachedBeforeAttach is the refusal for a partition that is no longer in
// the tree as the build listed it: detached, renamed, dropped, or replaced.
// The names the build captured may no longer name those relations, and a
// derived name does not prove this run made the index, so the error gives
// them as last-known names and asks an operator to inspect the catalog.
// INV: ST-7 — the build never attaches an index of a table it did not prove
// to be in the tree, and says what it may have left behind.
func (f *partitionedIndexFlow) detachedBeforeAttach(node partitionNode, child relationFacts, cause error) error {
	return fmt.Errorf("%w; the index last known as %s (%d) on the partition last known as %s (table %d) was not attached; inspect the catalog for it before dropping or keeping it",
		cause, qualifiedName(child.schema, child.name), child.oid, node.qualified(), node.oid)
}

const attachReadBackSQL = `SELECT EXISTS (
		   SELECT 1 FROM pg_catalog.pg_inherits h
		    WHERE h.inhrelid OPERATOR(pg_catalog.=) $1
		      AND h.inhparent OPERATOR(pg_catalog.=) $2)`

// proveNameBinding checks that the name the build resolved still names the
// same relation.
func proveNameBinding(ctx context.Context, q querier, want relationFacts) error {
	got, found, err := relationByName(ctx, q, want.schema, want.name)
	if err != nil {
		return err
	}
	if !found || got.oid != want.oid {
		return fmt.Errorf("%w: ST-7: after the attach, the name %s no longer resolves to index %d; a name resolved to another index",
			ErrInvariantViolation, qualifiedName(want.schema, want.name), want.oid)
	}
	return nil
}

// proveIdentity checks that the relation the build resolved under a name
// still has that identity: the same OID under the same name and schema, of
// the same kind, on the same table.
func (f *partitionedIndexFlow) proveIdentity(ctx context.Context, q querier, want relationFacts, tableOID uint32) error {
	got, found, err := relationByOID(ctx, q, want.oid)
	if err != nil {
		return err
	}
	if !found || got.schema != want.schema || got.name != want.name || got.relkind != want.relkind || got.tableOID != tableOID {
		return fmt.Errorf("%w: ST-7: index %s no longer has the identity the build resolved",
			ErrInvariantViolation, qualifiedName(want.schema, want.name))
	}
	return nil
}
