package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The leaf step of adding a PRIMARY KEY or UNIQUE constraint to a partitioned
// table online. PostgreSQL refuses ADD CONSTRAINT ... USING INDEX on a
// partitioned table, and a plain ADD CONSTRAINT builds every partition's
// index while it holds ACCESS EXCLUSIVE on the tree. A plain ADD CONSTRAINT
// on the parent adopts a partition's constraint of the same definition
// instead of building one, so the flow gives every leaf that constraint
// first: a concurrent build of the unique index, then ADD CONSTRAINT ...
// USING INDEX, which only flips the catalog. This file is that step for one
// leaf.

// partitionedKeyKind is the constraint kind the step adds.
type partitionedKeyKind int

const (
	partitionedPrimaryKey partitionedKeyKind = iota + 1
	partitionedUniqueKey
)

// contype is the kind as pg_constraint.contype records it.
func (k partitionedKeyKind) contype() string {
	if k == partitionedPrimaryKey {
		return "p"
	}
	return "u"
}

// clause is the kind as ALTER TABLE spells it.
func (k partitionedKeyKind) clause() string {
	if k == partitionedPrimaryKey {
		return "PRIMARY KEY"
	}
	return "UNIQUE"
}

// partitionedKeySpec is the requested constraint: its kind, the parent's
// constraint name, and the key columns in order. It carries nothing else, so
// INCLUDE, NULLS NOT DISTINCT, DEFERRABLE, storage parameters, a tablespace,
// and expression keys cannot reach the step.
type partitionedKeySpec struct {
	kind    partitionedKeyKind
	name    string
	columns []string
}

// validate refuses a spec the step cannot act on.
func (s partitionedKeySpec) validate() error {
	if s.kind != partitionedPrimaryKey && s.kind != partitionedUniqueKey {
		return fmt.Errorf("%w: unknown key constraint kind %d", ErrInvariantViolation, s.kind)
	}
	if s.name == "" {
		return fmt.Errorf("%w: the key constraint has no name", ErrInvariantViolation)
	}
	if len(s.columns) == 0 {
		return fmt.Errorf("%w: key constraint %s has no columns", ErrInvariantViolation, s.name)
	}
	seen := make(map[string]struct{}, len(s.columns))
	for _, c := range s.columns {
		if _, dup := seen[c]; dup {
			return fmt.Errorf("%w: key constraint %s names column %q twice", ErrInvariantViolation, s.name, c)
		}
		seen[c] = struct{}{}
	}
	return nil
}

// leafKeyAction says what the step did to the leaf.
type leafKeyAction string

const (
	// leafKeyFound: the leaf already had the key constraint; nothing changed.
	leafKeyFound leafKeyAction = "found"
	// leafKeyConstrained: an index the leaf already had was given the
	// constraint.
	leafKeyConstrained leafKeyAction = "constrained"
	// leafKeyBuilt: the index was built concurrently, then given the
	// constraint.
	leafKeyBuilt leafKeyAction = "built"
)

// leafKeyResult is the leaf's key constraint once the step is done.
type leafKeyResult struct {
	action        leafKeyAction
	constraint    string
	constraintOID uint32
	index         string
	indexOID      uint32
	// recoveredDebris is whether the step removed an earlier build's
	// leftover before it built.
	recoveredDebris bool
}

// ensureLeafKey brings one leaf partition to the requested key: a PRIMARY KEY
// or UNIQUE constraint on exactly the key columns, backed by a valid index of
// that definition, attached to no parent constraint. It reads the leaf and
// decides every call, so a rerun resumes from whatever an earlier call left:
//
//   - a matching constraint, under any name, is the result as it stands;
//   - a valid unique index of the key's definition, under the derived name or
//     any other and attached to nothing, is given the constraint;
//   - an invalid one under the derived name, an earlier build's debris, is
//     removed under proof (recoverAbandonedIndex) and built again;
//   - otherwise the index is built concurrently under the derived name and
//     given the constraint.
//
// The constraint takes the index's name, so ADD CONSTRAINT ... USING INDEX
// renames nothing. Anything else under the derived name, a conflicting
// constraint, and a primary key over a nullable column are refused, typed,
// before anything is created; nothing the step did not create is dropped or
// renamed. The caller holds the parent's advisory table lock (LK-1), and the
// pool needs the concurrent build's two connections.
func ensureLeafKey(ctx context.Context, pool *pgxpool.Pool, spec partitionedKeySpec, node partitionNode, b ConcurrentBudget) (leafKeyResult, error) {
	if err := spec.validate(); err != nil {
		return leafKeyResult{}, err
	}
	if node.relkind != "r" {
		return leafKeyResult{}, fmt.Errorf("%w: partition %s is of kind %q, and the key step takes a leaf partition",
			ErrInvariantViolation, node.qualified(), node.relkind)
	}
	tx := briefTx{pool: pool}
	name := derivedPartitionIndexName(spec.name, node.oid)
	var facts leafKeyFacts
	err := tx.read(ctx, "inspect the key of partition "+node.qualified(), func(ctx context.Context, q querier) error {
		var err error
		facts, err = readLeafKeyFacts(ctx, q, node, spec, name)
		return err
	})
	if err != nil {
		return leafKeyResult{}, err
	}
	plan, err := decideLeafKey(spec, node, name, facts)
	if err != nil {
		return leafKeyResult{}, err
	}
	want := leafKeyDefinitionKey(facts.columns)

	var result leafKeyResult
	idx := plan.index
	switch plan.state {
	case leafKeyDone:
		return leafKeyResult{
			action:     leafKeyFound,
			constraint: plan.constraint.name, constraintOID: plan.constraint.oid,
			index: plan.index.name, indexOID: plan.index.oid,
		}, nil
	case leafKeyConstraintPending:
		result.action = leafKeyConstrained
	case leafKeyBuildNeeded:
		result.action = leafKeyBuilt
		result.recoveredDebris = plan.debris
		if idx, err = buildLeafKeyIndex(ctx, pool, tx, spec, node, name, want, plan.debris, b); err != nil {
			return leafKeyResult{}, err
		}
	default:
		return leafKeyResult{}, fmt.Errorf("%w: unknown leaf key state %d", ErrInvariantViolation, plan.state)
	}
	constraintOID, err := addLeafKeyConstraint(ctx, tx, spec, node, idx, want)
	if err != nil {
		return leafKeyResult{}, err
	}
	result.constraint, result.constraintOID = idx.name, constraintOID
	result.index, result.indexOID = idx.name, idx.oid
	return result, nil
}

// leafKeyIndexSQL is the concurrent build of the key's index on the leaf:
// unique, btree, the key columns in order, everything else at its default.
// Every identifier is quoted.
func leafKeyIndexSQL(spec partitionedKeySpec, node partitionNode, name string) string {
	columns := make([]string, 0, len(spec.columns))
	for _, c := range spec.columns {
		columns = append(columns, pgx.Identifier{c}.Sanitize())
	}
	return "CREATE UNIQUE INDEX CONCURRENTLY " + pgx.Identifier{name}.Sanitize() +
		" ON " + pgx.Identifier{node.schema, node.name}.Sanitize() +
		" USING btree (" + strings.Join(columns, ", ") + ")"
}

// buildLeafKeyIndex builds the key's index on the leaf under the derived name,
// first removing an earlier build's debris when the plan found some, and
// returns it as the catalog reads it after the build.
func buildLeafKeyIndex(ctx context.Context, pool *pgxpool.Pool, tx briefTx, spec partitionedKeySpec, node partitionNode, name, want string, debris bool, b ConcurrentBudget) (relationFacts, error) {
	// The leaf was read before this call decided to build. A partition
	// renamed, detached, or replaced since must not get an index under the
	// name it used to have (INV: ST-7); the build repeats the check on its
	// own session against the leaf's OID.
	err := tx.read(ctx, "prove partition "+node.qualified()+" is still in the tree", func(ctx context.Context, q querier) error {
		return proveNodeIn(ctx, q, node)
	})
	if err != nil {
		return relationFacts{}, err
	}
	buildSQL := leafKeyIndexSQL(spec, node, name)
	if debris {
		// The invalid index under the derived name, or the quarantined
		// debris of an earlier recovery, is proved abandoned under the
		// leaf's own lock, quarantined by OID, and dropped; a backend still
		// building it refuses the step (INV: LK-5).
		if _, err := recoverAbandonedIndex(ctx, pool, buildSQL, b, recoveryPool{
			minConns: dropRecoveryMinConns,
			sessions: "recovery session and drop session",
		}, nil); err != nil {
			return relationFacts{}, fmt.Errorf("recover the invalid index %s on partition %s: %w",
				qualifiedName(node.schema, name), node.qualified(), err)
		}
	}
	built, err := buildIndexConcurrentlyOn(ctx, pool, buildSQL, b, nil, node.oid)
	if err != nil {
		return relationFacts{}, fmt.Errorf("build the key index of partition %s: %w", node.qualified(), err)
	}
	var idx relationFacts
	err = tx.read(ctx, "read the key index built on partition "+node.qualified(), func(ctx context.Context, q querier) error {
		var found bool
		var err error
		if idx, found, err = relationByOID(ctx, q, built.IndexOID); err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: index %s built on partition %s is not in the catalog",
				ErrInvariantViolation, qualifiedName(node.schema, name), node.qualified())
		}
		return requireLeafKeyIndex(node, idx, want)
	})
	return idx, err
}

// requireLeafKeyIndex proves that an index is one the leaf's key constraint
// can take as it stands: a valid index on the leaf, of the key's definition,
// attached to nothing, and backing no constraint yet.
func requireLeafKeyIndex(node partitionNode, idx relationFacts, want string) error {
	name := qualifiedName(idx.schema, idx.name)
	if idx.relkind != "i" || idx.tableOID != node.oid {
		return fmt.Errorf("%w: ST-7: %s is %s, not an index on partition %s",
			ErrInvariantViolation, name, describeRelation(idx), node.qualified())
	}
	if !idx.valid || idx.isPartition || idx.backsConstraint {
		return fmt.Errorf("%w: index %s on partition %s is no longer a valid index attached to nothing and backing no constraint",
			ErrInvariantViolation, name, node.qualified())
	}
	key, err := idx.definitionKey()
	if err != nil {
		return err
	}
	if key != want {
		return fmt.Errorf("%w: index %s on partition %s does not have the key's definition",
			ErrInvariantViolation, name, node.qualified())
	}
	return nil
}

const leafKeyReadBackSQL = `SELECT k.oid, k.contype::pg_catalog.text, k.conindid, k.conparentid
		   FROM pg_catalog.pg_constraint k
		  WHERE k.conrelid OPERATOR(pg_catalog.=) $1
		    AND k.conname OPERATOR(pg_catalog.=) $2`

// addLeafKeyConstraint runs ALTER TABLE leaf ADD CONSTRAINT idx {PRIMARY KEY |
// UNIQUE} USING INDEX idx and returns the constraint's OID. The statement
// takes ACCESS EXCLUSIVE on the leaf until the transaction ends and changes
// only the catalog: the index is already built and valid, the constraint
// takes the index's name so nothing is renamed, and a primary key's columns
// are proved NOT NULL first. It runs in one brief transaction under the
// session's lock_timeout and statement_timeout. A rerun after an unknown
// commit outcome finds the constraint and reports it.
func addLeafKeyConstraint(ctx context.Context, tx briefTx, spec partitionedKeySpec, node partitionNode, idx relationFacts, want string) (uint32, error) {
	alterSQL := "ALTER TABLE " + pgx.Identifier{node.schema, node.name}.Sanitize() +
		" ADD CONSTRAINT " + pgx.Identifier{idx.name}.Sanitize() + " " + spec.kind.clause() +
		" USING INDEX " + pgx.Identifier{idx.name}.Sanitize()
	var constraintOID uint32
	err := tx.run(ctx, "add the key constraint of partition "+node.qualified(), catalogReadPath(), true,
		func(ctx context.Context, tx pgx.Tx) error {
			// The partition stays in the tree, under its name, until the
			// transaction ends (INV: ST-7).
			if err := lockAndProveNode(ctx, tx, node); err != nil {
				if errors.Is(err, ErrInvariantViolation) {
					return fmt.Errorf("%w; the index last known as %s (%d) on the partition last known as %s (table %d) was not given its constraint; inspect the catalog for it before dropping or keeping it",
						err, qualifiedName(idx.schema, idx.name), idx.oid, node.qualified(), node.oid)
				}
				return err
			}
			// The statement names the index and the server resolves the name,
			// so the index under it must still be the one decided on, and
			// still fit the constraint (INV: ST-7).
			if err := proveIdentity(ctx, tx, idx, node.oid); err != nil {
				return err
			}
			current, found, err := relationByOID(ctx, tx, idx.oid)
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("%w: ST-7: index %s is gone", ErrInvariantViolation, qualifiedName(idx.schema, idx.name))
			}
			if err := requireLeafKeyIndex(node, current, want); err != nil {
				return err
			}
			// INV: LK-2 — a primary key over a nullable column would set NOT
			// NULL and scan the partition under ACCESS EXCLUSIVE. The lock
			// held since the proof above excludes ALTER COLUMN ... DROP NOT
			// NULL, so the columns read here stay NOT NULL through the
			// statement.
			if spec.kind == partitionedPrimaryKey {
				columns, err := readLeafKeyColumns(ctx, tx, node, spec.columns)
				if err != nil {
					return err
				}
				if err := requireNotNullForPrimaryKey(spec, node, columns); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, alterSQL); err != nil {
				return err
			}
			// What the statement did is read back by identity before the
			// transaction commits: a constraint of the requested kind under
			// the index's name, backed by that index, attached to no parent
			// constraint, and the index still under its own name (INV:
			// ST-7). Any other outcome rolls the step back.
			var (
				contype   string
				conindid  uint32
				conparent uint32
			)
			if err := tx.QueryRow(ctx, leafKeyReadBackSQL, node.oid, idx.name).Scan(&constraintOID, &contype, &conindid, &conparent); err != nil {
				return fmt.Errorf("read back the key constraint %s of partition %s: %w", idx.name, node.qualified(), err)
			}
			if contype != spec.kind.contype() || conindid != idx.oid || conparent != 0 {
				return fmt.Errorf("%w: ST-7: after ADD CONSTRAINT, constraint %s on partition %s is not the requested key backed by index %d",
					ErrInvariantViolation, idx.name, node.qualified(), idx.oid)
			}
			return proveIdentity(ctx, tx, idx, node.oid)
		})
	if err != nil {
		return 0, err
	}
	return constraintOID, nil
}
