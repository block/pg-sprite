package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/progress"
)

// partitionedFKFlow is one run of the partitioned foreign key build under the
// parent's table lock. The parent's step is in partitioned_fk_parent.go.
type partitionedFKFlow struct {
	tx      briefTx
	pool    *pgxpool.Pool
	budget  ValidateBudget
	tracker *progress.Tracker
	// runStep is the step of the run that owns the tracker; every report
	// goes to it.
	runStep int
	req     partitionedFKRequest
	target  indexTarget
	fk      resolvedFK

	// leaves is each leaf's foreign key as its latest leaf step left it, by
	// the leaf's OID: the foreign key the parent's step proves is still
	// there before it adds the parent's.
	leaves map[uint32]leafFKResult
	rep    PartitionedForeignKeyReport
}

// runPartitionedFK is the build's body, run under the context the table lock
// is bound to.
func runPartitionedFK(ctx context.Context, pool *pgxpool.Pool, req partitionedFKRequest, b ValidateBudget, tracker *progress.Tracker, runStep int) (PartitionedForeignKeyReport, error) {
	f := &partitionedFKFlow{
		tx: briefTx{pool: pool}, pool: pool, budget: b, tracker: tracker, runStep: runStep, req: req,
		leaves: map[uint32]leafFKResult{},
		rep:    PartitionedForeignKeyReport{Schema: req.schema, Table: req.table, Constraint: req.spec.name},
	}
	err := f.run(ctx)
	return f.rep, err
}

func (f *partitionedFKFlow) run(ctx context.Context) error {
	if err := f.resolve(ctx); err != nil {
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
	passes, err := settlePasses(ctx, maxPartitionedForeignKeyPasses, ErrPartitionedForeignKeyUnsettled, func(ctx context.Context, pass int) (bool, error) {
		if pass > 1 {
			var err error
			if tree, err = f.readTree(ctx); err != nil {
				return false, err
			}
		}
		if err := f.ensureLeaves(ctx, tree); err != nil {
			return false, err
		}
		added, err := f.addParentFK(ctx)
		// A pass that could not add the parent's foreign key found a
		// partition it had not covered, and the next pass covers it.
		return !added, err
	})
	f.rep.Passes = passes
	if errors.Is(err, ErrPartitionedForeignKeyUnsettled) {
		return fmt.Errorf("%w: %s still had partitions without the foreign key after %d passes",
			err, qualifiedName(f.req.schema, f.req.table), maxPartitionedForeignKeyPasses)
	}
	return err
}

// resolve re-proves, under the lock, what the preflight proved before it:
// the table the statement names exists and is partitioned. In the same read
// it refuses a server that has the native route, and resolves the
// referenced table and its columns.
func (f *partitionedFKFlow) resolve(ctx context.Context) error {
	return f.tx.read(ctx, "resolve the partitioned table and the referenced table", func(ctx context.Context, q querier) error {
		var versionNum int
		if err := q.QueryRow(ctx, `SELECT pg_catalog.current_setting('server_version_num')::pg_catalog.int4`).Scan(&versionNum); err != nil {
			return fmt.Errorf("read server_version_num: %w", err)
		}
		major := versionNum / 10000
		if major >= partitionedFKNotValidMajor {
			return fmt.Errorf("%w: PostgreSQL %d; add %s with ADD FOREIGN KEY ... NOT VALID, then VALIDATE CONSTRAINT",
				ErrPartitionedForeignKeyNotValidSupported, major, f.req.spec.name)
		}
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
		ref, err := resolveFKReference(ctx, q, f.req.spec)
		if err != nil {
			return err
		}
		f.target, f.fk = target, resolvedFK{spec: f.req.spec, ref: ref, serverMajor: major}
		return nil
	})
}

// checkParent reads what the parent already has and decides whether the
// build may start (false), the parent already carries exactly the requested
// foreign key adopted on every partition, which the build reports as done
// (true), or something stands in the way (a typed refusal). It runs before
// any partition is touched, so a refusal leaves the tree as it was, and a run
// resumed after the parent's foreign key was added does not mistake the
// adopted partitions' foreign keys for conflicts.
func (f *partitionedFKFlow) checkParent(ctx context.Context) (bool, error) {
	parent := qualifiedName(f.target.schema, f.target.table)
	var done bool
	err := f.tx.read(ctx, "inspect the constraints of "+parent, func(ctx context.Context, q querier) error {
		attnums, err := fkColumnAttnums(ctx, q, f.target.tableOID, parent, f.req.spec.columns)
		if err != nil {
			return err
		}
		fks, err := readForeignKeys(ctx, q, f.target.tableOID, parent, f.fk.serverMajor)
		if err != nil {
			return err
		}
		var nameHeld bool
		if err := q.QueryRow(ctx, fkConstraintNamedSQL, f.target.tableOID, f.req.spec.name).Scan(&nameHeld); err != nil {
			return fmt.Errorf("look for constraint %s on %s: %w", f.req.spec.name, parent, err)
		}
		existing, found, err := decideParentFK(parent, f.fk, attnums, fks, nameHeld)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		partitions, err := requireAdoptedFKEverywhere(ctx, q, f.target, existing)
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

// decideParentFK judges the parent's foreign keys: the requested one,
// validated, under the requested name is an earlier run's finished work
// (found); anything else under the name, or the requested foreign key under
// another name, is refused; otherwise the build starts.
func decideParentFK(parent string, fk resolvedFK, attnums []int16, fks []catalogFK, nameHeld bool) (catalogFK, bool, error) {
	matches := requestedForeignKeys(fks, fk.spec, attnums, fk.ref)
	for _, k := range matches {
		if k.name == fk.spec.name && k.validated {
			return k, true, nil
		}
	}
	if nameHeld {
		return catalogFK{}, false, fmt.Errorf("%w: %s already has a constraint named %s that is not the requested validated foreign key",
			ErrPartitionedForeignKeyParentConflict, parent, fk.spec.name)
	}
	if len(matches) > 0 {
		return catalogFK{}, false, fmt.Errorf("%w: %s already has the requested foreign key as %s",
			ErrPartitionedForeignKeyParentConflict, parent, matches[0].name)
	}
	return catalogFK{}, false, nil
}

// requireAdoptedFKEverywhere proves every partition has a foreign key the
// parent's existing foreign key adopted, and returns how many partitions
// the tree holds.
func requireAdoptedFKEverywhere(ctx context.Context, q querier, target indexTarget, existing catalogFK) (int, error) {
	tree, err := listPartitionTree(ctx, q, target.tableOID)
	if err != nil {
		return 0, err
	}
	for _, n := range tree {
		adopted, err := adoptedFKs(ctx, q, n, existing.oid)
		if err != nil {
			return 0, err
		}
		if len(adopted) != 1 {
			return 0, fmt.Errorf("%w: partition %s has %d foreign keys adopted by %s on %s, not one",
				ErrInvariantViolation, n.qualified(), len(adopted), existing.name, qualifiedName(target.schema, target.table))
		}
	}
	return len(tree), nil
}

// readTree lists the partition tree, refuses a tree the build does not take,
// and refuses a leaf whose foreign keys stand in the way. It is read at the
// start and on every later pass, so a foreign table, a sub-partitioned
// partition, or a conflicting leaf attached mid-run refuses the run too.
func (f *partitionedFKFlow) readTree(ctx context.Context) ([]partitionNode, error) {
	var tree []partitionNode
	err := f.tx.read(ctx, "list the partition tree", func(ctx context.Context, q querier) error {
		var err error
		tree, err = listPartitionTree(ctx, q, f.target.tableOID)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := refuseFKTree(f.target, f.fk.ref, tree); err != nil {
		return nil, err
	}
	if err := f.refuseLeafConflicts(ctx, tree); err != nil {
		return nil, err
	}
	f.rep.Partitions = len(tree)
	return tree, nil
}

// refuseLeafConflicts decides every leaf from one read-only transaction
// before the pass's first leaf step, and returns the first refusal a leaf
// step would meet: two foreign keys of the requested definition, one
// attached to a parent's, one the parent's statement could adopt that has an
// ON DELETE column list, or the derived name held by another constraint. So
// a conflict on a late leaf refuses the pass before any earlier leaf is
// given the foreign key. Each leaf step still decides again, so a leaf that
// changes after this read is refused at its own step.
func (f *partitionedFKFlow) refuseLeafConflicts(ctx context.Context, tree []partitionNode) error {
	return f.tx.read(ctx, "inspect the foreign keys of every partition", func(ctx context.Context, q querier) error {
		for _, n := range tree {
			if _, err := readAndDecideLeafFK(ctx, q, f.fk, n, derivedPartitionIndexName(f.fk.spec.name, n.oid)); err != nil {
				return err
			}
		}
		return nil
	})
}

// refuseFKTree refuses a tree holding a foreign table, which cannot carry a
// foreign key, a sub-partitioned partition, a partition whose detach did not
// finish, or the referenced table itself.
func refuseFKTree(target indexTarget, ref fkReference, tree []partitionNode) error {
	parent := qualifiedName(target.schema, target.table)
	if foreign := foreignPartitions(tree); len(foreign) > 0 {
		return fmt.Errorf("%w: %s under %s", ErrForeignTablePartition, strings.Join(foreign, ", "), parent)
	}
	if pending := detachPendingPartitions(tree); len(pending) > 0 {
		return fmt.Errorf("%w: %s under %s", ErrPartitionDetachPending, strings.Join(pending, ", "), parent)
	}
	var nested []string
	for _, n := range tree {
		if n.subPartitioned() {
			nested = append(nested, n.qualified())
		}
		if n.oid == ref.oid {
			return fmt.Errorf("%w: %s is a partition of %s", ErrPartitionedForeignKeyReferencedTable, qualifiedName(ref.schema, ref.table), parent)
		}
	}
	if len(nested) > 0 {
		return fmt.Errorf("%w: %s under %s", ErrPartitionedForeignKeySubPartitioned, strings.Join(nested, ", "), parent)
	}
	return nil
}

// ensureLeaves gives every leaf in the tree the foreign key, in order, and
// records what each step left. A leaf's first result is the one the report
// keeps: a later pass finds what the first one made.
func (f *partitionedFKFlow) ensureLeaves(ctx context.Context, tree []partitionNode) error {
	for _, n := range tree {
		res, err := ensureLeafForeignKey(ctx, f.pool, f.fk, n, f.budget, f.startStep)
		if err != nil {
			return err
		}
		if _, seen := f.leaves[n.oid]; !seen {
			action, err := res.action.reported()
			if err != nil {
				return err
			}
			f.rep.Leaves = append(f.rep.Leaves, PartitionForeignKeyResult{
				Schema: n.schema, Table: n.name,
				Constraint: res.constraint, ConstraintOID: res.constraintOID,
				Action: action,
			})
		}
		f.leaves[n.oid] = res
	}
	return nil
}

// reported is the leaf step's action as the report names it.
func (a leafFKAction) reported() (PartitionForeignKeyAction, error) {
	switch a {
	case leafFKFound:
		return PartitionForeignKeyFound, nil
	case leafFKValidated:
		return PartitionForeignKeyValidated, nil
	case leafFKAdded:
		return PartitionForeignKeyAdded, nil
	default:
		return "", fmt.Errorf("%w: unknown leaf foreign key action %q", ErrInvariantViolation, a)
	}
}

// startStep reports the statement in flight on the run's step.
func (f *partitionedFKFlow) startStep(op progress.Operation, sql string) {
	if f.tracker == nil {
		return
	}
	f.tracker.StartStep(f.runStep, op, sql)
}
