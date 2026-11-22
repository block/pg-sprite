package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/statement"
)

// partitionedIndexFlow is one run of the build under the parent's table lock.
// Its fields are the run's facts and progress; the steps are in this file and in
// partitioned_index_partition.go.
type partitionedIndexFlow struct {
	tx      briefTx
	pool    *pgxpool.Pool
	budget  ConcurrentBudget
	tracker *progress.Tracker
	sql     string
	req     partitionedIndexRequest
	target  indexTarget

	// parent is the parent's index as the catalog reads it right after it is
	// created.
	parent relationFacts
	// rootDef is the parent's index definition read under catalogReadPath: the
	// text every partition's statement is derived from.
	rootDef string

	// changes counts the catalog changes the run has made; a pass that leaves
	// it where it found it has nothing left to do.
	changes int
	// step is the progress step in flight; seen is the partitions already
	// counted into the tracker's total.
	step int
	seen map[uint32]struct{}
	rep  PartitionedIndexReport
}

// runPartitionedIndex is the build's body, run under the context the table lock
// is bound to.
func runPartitionedIndex(ctx context.Context, pool *pgxpool.Pool, req partitionedIndexRequest, sql string, b ConcurrentBudget, tracker *progress.Tracker) (PartitionedIndexReport, error) {
	f := &partitionedIndexFlow{
		tx: briefTx{pool: pool}, pool: pool, budget: b, tracker: tracker, sql: sql, req: req,
		seen: map[uint32]struct{}{},
		rep:  PartitionedIndexReport{Schema: req.schema, Index: req.index},
	}
	err := f.run(ctx)
	return f.rep, err
}

func (f *partitionedIndexFlow) run(ctx context.Context) error {
	if err := f.resolveTarget(ctx); err != nil {
		return err
	}
	tree, err := f.readTree(ctx)
	if err != nil {
		return err
	}
	if err := f.createParentIndex(ctx); err != nil {
		return err
	}
	passes, err := settlePasses(ctx, maxPartitionedIndexPasses, func(ctx context.Context, pass int) (bool, error) {
		if pass > 1 {
			var err error
			if tree, err = f.readTree(ctx); err != nil {
				return false, err
			}
		}
		before := f.changes
		if err := f.processChildren(ctx, f.target.tableOID, f.parent, tree); err != nil {
			return false, err
		}
		return f.changes != before, nil
	})
	f.rep.Passes = passes
	if errors.Is(err, ErrPartitionedIndexUnsettled) {
		return fmt.Errorf("%w: %s still had partitions without an index after %d passes",
			err, qualifiedName(f.req.schema, f.req.table), maxPartitionedIndexPasses)
	}
	if err != nil {
		return err
	}
	return f.verify(ctx)
}

// resolveTarget re-reads, under the lock, what the preflight proved before it:
// the table the statement names exists and is partitioned.
func (f *partitionedIndexFlow) resolveTarget(ctx context.Context) error {
	return f.tx.read(ctx, "resolve the partitioned table", func(ctx context.Context, q querier) error {
		target, err := resolveTarget(ctx, q, concurrentIndexBuild{index: f.req.index, tableSchema: f.req.schema, table: f.req.table})
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
		f.target = target
		return nil
	})
}

const relkindSQL = `SELECT c.relkind::pg_catalog.text FROM pg_catalog.pg_class c WHERE c.oid OPERATOR(pg_catalog.=) $1`

// readTree lists the partition tree and refuses it when it holds a foreign
// table: a foreign table cannot carry an index, and a parent's index never
// becomes valid while one of its partitions has none. It is read at the start
// and on every later pass, so a foreign table attached mid-run refuses the run
// too.
func (f *partitionedIndexFlow) readTree(ctx context.Context) ([]partitionNode, error) {
	var tree []partitionNode
	err := f.tx.read(ctx, "list the partition tree", func(ctx context.Context, q querier) error {
		var err error
		tree, err = listPartitionTree(ctx, q, f.target.tableOID)
		return err
	})
	if err != nil {
		return nil, err
	}
	if foreign := foreignPartitions(tree); len(foreign) > 0 {
		return nil, fmt.Errorf("%w: %s under %s",
			ErrForeignTablePartition, strings.Join(foreign, ", "), qualifiedName(f.target.schema, f.target.table))
	}
	f.countNewPartitions(tree)
	f.rep.Partitions = len(tree)
	return tree, nil
}

// countNewPartitions raises the tracker's total by the partitions it has not
// counted yet.
func (f *partitionedIndexFlow) countNewPartitions(tree []partitionNode) {
	added := 0
	for _, n := range tree {
		if _, ok := f.seen[n.oid]; !ok {
			f.seen[n.oid] = struct{}{}
			added++
		}
	}
	if f.tracker != nil {
		f.tracker.AddSteps(added)
	}
}

// startStep moves the tracker to the next step, or stays on the current one
// when the statement is the second of the same partition.
func (f *partitionedIndexFlow) startStep(op progress.Operation, sql string, next bool) {
	if next {
		f.step++
	}
	if f.tracker != nil {
		f.tracker.StartStep(f.step, op, sql)
	}
}

// onlyTable is the target as createOnlyIndex checks it.
func (f *partitionedIndexFlow) onlyTable() onlyIndexTable {
	return onlyIndexTable{oid: f.target.tableOID, schema: f.target.schema, name: f.target.table}
}

// createParentIndex creates the parent's index with ON ONLY.
func (f *partitionedIndexFlow) createParentIndex(ctx context.Context) error {
	onlySQL, err := statement.OnlyIndex(f.sql)
	if err != nil {
		return fmt.Errorf("make the parent index statement for %s: %w", qualifiedName(f.target.schema, f.req.index), err)
	}
	f.startStep(progress.OperationBrief, onlySQL, true)
	// The statement's own expressions resolve under the target's schema, then
	// public, the same path a concurrent build pins.
	path := []string{f.target.schema, "public"}
	err = f.tx.run(ctx, "create the parent index", path, true, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		f.parent, err = createOnlyIndex(ctx, tx, f.onlyTable(), f.target.schema, f.req.index, onlySQL)
		return err
	})
	if err != nil {
		return err
	}
	f.rootDef = f.parent.def
	f.rep.IndexOID = f.parent.oid
	f.rep.ParentCreated = true
	return nil
}

// verify re-reads the parent's index and reports success only when it is valid:
// PostgreSQL marks it so once every partition has an index attached, and an
// index left invalid after the build attached them all is a state this executor
// does not understand.
func (f *partitionedIndexFlow) verify(ctx context.Context) error {
	return f.tx.read(ctx, "verify the parent index", func(ctx context.Context, q querier) error {
		parent, found, err := relationByOID(ctx, q, f.parent.oid)
		if err != nil {
			return err
		}
		name := qualifiedName(f.target.schema, f.req.index)
		if !found || parent.schema != f.target.schema || parent.name != f.req.index || !parent.isPartitionedIndex() {
			return fmt.Errorf("%w: parent index %s no longer has the identity the build resolved", ErrInvariantViolation, name)
		}
		if !parent.valid {
			return fmt.Errorf("%w: parent index %s is invalid although every partition has an attached index",
				ErrInvariantViolation, name)
		}
		if err := q.QueryRow(ctx, `SELECT pg_catalog.current_setting('server_version')`).Scan(&f.rep.ServerVersion); err != nil {
			return fmt.Errorf("read server_version: %w", err)
		}
		return nil
	})
}

// settlePasses runs walk until a pass changes nothing, at most limit times, and
// returns how many passes ran. A walk that still changes something on the last
// pass has not settled: another session keeps adding partitions faster than the
// walks cover them.
func settlePasses(ctx context.Context, limit int, walk func(ctx context.Context, pass int) (changed bool, err error)) (int, error) {
	for pass := 1; pass <= limit; pass++ {
		changed, err := walk(ctx, pass)
		if err != nil {
			return pass, err
		}
		if !changed {
			return pass, nil
		}
	}
	return limit, ErrPartitionedIndexUnsettled
}
