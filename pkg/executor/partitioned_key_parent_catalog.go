package executor

import (
	"context"
	"fmt"
	"strings"
)

// The catalog reads and the decision of what the partitioned table already
// carries, made before the build touches a partition. Every read runs under
// catalogReadPath with every relation and operator pg_catalog-qualified
// (CO-9).

// parentConstraint is one constraint of the partitioned table.
type parentConstraint struct {
	oid        uint32
	name       string
	contype    string
	deferrable bool
	key        []int16
	// index is the index behind a key constraint; hasIndex is false for a
	// constraint without one.
	index    relationFacts
	hasIndex bool
}

const parentConstraintsSQL = `SELECT k.oid, k.conname, k.contype::pg_catalog.text, k.condeferrable, k.conindid, k.conkey
		   FROM pg_catalog.pg_constraint k
		  WHERE k.conrelid OPERATOR(pg_catalog.=) $1
		  ORDER BY k.oid`

// readParentConstraints lists every constraint of the table and reads the
// index behind each one that has an index.
func readParentConstraints(ctx context.Context, q querier, tableOID uint32) ([]parentConstraint, error) {
	rows, err := q.Query(ctx, parentConstraintsSQL, tableOID)
	if err != nil {
		return nil, fmt.Errorf("list the constraints of table %d: %w", tableOID, err)
	}
	var (
		found    []parentConstraint
		indexOID []uint32
	)
	for rows.Next() {
		var (
			c       parentConstraint
			conindx uint32
		)
		if err := rows.Scan(&c.oid, &c.name, &c.contype, &c.deferrable, &conindx, &c.key); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan the constraints of table %d: %w", tableOID, err)
		}
		found = append(found, c)
		indexOID = append(indexOID, conindx)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the constraints of table %d: %w", tableOID, err)
	}
	for i := range found {
		if indexOID[i] == 0 {
			continue
		}
		idx, ok, err := relationByOID(ctx, q, indexOID[i])
		if err != nil {
			return nil, err
		}
		found[i].index, found[i].hasIndex = idx, ok
	}
	return found, nil
}

// isRequestedKey reports whether the parent's constraint is exactly the
// requested key: of its kind, not deferrable, on the key's columns in order,
// and backed by a valid partitioned index of the key's definition.
func (c parentConstraint) isRequestedKey(spec partitionedKeySpec, columns []leafKeyColumn, want string) (bool, error) {
	if c.contype != spec.kind.contype() || c.deferrable || !sameKeyColumns(c.key, columns) {
		return false, nil
	}
	if !c.hasIndex || !c.index.isPartitionedIndex() || !c.index.valid {
		return false, nil
	}
	key, err := c.index.definitionKey()
	if err != nil {
		return false, err
	}
	return key == want, nil
}

// decideParentKey decides from one snapshot of the parent whether it already
// has the requested constraint (found), may be given it, or holds something
// that stands in the way. holder is the relation under the constraint's name
// in the parent's schema, which the parent's index would take.
func decideParentKey(spec partitionedKeySpec, parent partitionNode, columns []leafKeyColumn, constraints []parentConstraint, holder relationFacts, held bool) (parentConstraint, bool, error) {
	want := leafKeyDefinitionKey(columns)
	var named *parentConstraint
	for i, c := range constraints {
		if c.name == spec.name {
			named = &constraints[i]
			continue
		}
		if spec.kind == partitionedPrimaryKey && c.contype == partitionedPrimaryKey.contype() {
			return parentConstraint{}, false, fmt.Errorf("%w: %s already has primary key %s",
				ErrPartitionedKeyParentConflict, parent.qualified(), c.name)
		}
	}
	if named != nil {
		equal, err := named.isRequestedKey(spec, columns, want)
		if err != nil {
			return parentConstraint{}, false, err
		}
		if !equal {
			return parentConstraint{}, false, fmt.Errorf("%w: %s already has a constraint named %s that is not the requested %s",
				ErrPartitionedKeyParentConflict, parent.qualified(), spec.name, spec.kind.clause())
		}
		return *named, true, nil
	}
	if held {
		return parentConstraint{}, false, fmt.Errorf("%w: %s, which the constraint's index on %s would be named, is already %s",
			ErrPartitionedKeyParentConflict, qualifiedName(holder.schema, holder.name), parent.qualified(), describeRelation(holder))
	}
	if err := requireParentNotNull(spec, parent, columns); err != nil {
		return parentConstraint{}, false, err
	}
	return parentConstraint{}, false, nil
}

// requireParentNotNull refuses a primary key over a column the partitioned
// table allows to be NULL. The parent's ADD CONSTRAINT would set NOT NULL on
// it and on every partition, which scans a partition that allows NULL under
// ACCESS EXCLUSIVE; a partition of a NOT NULL column is NOT NULL too, so the
// parent's columns decide for the whole tree.
func requireParentNotNull(spec partitionedKeySpec, parent partitionNode, columns []leafKeyColumn) error {
	if spec.kind != partitionedPrimaryKey {
		return nil
	}
	var nullable []string
	for _, c := range columns {
		if !c.notNull {
			nullable = append(nullable, c.name)
		}
	}
	if len(nullable) == 0 {
		return nil
	}
	return fmt.Errorf("%w: partitioned table %s allows NULL in %s; make the columns NOT NULL first",
		ErrPartitionedKeyNullableColumn, parent.qualified(), strings.Join(nullable, ", "))
}

const unadoptedPartitionsSQL = `SELECT n.nspname, c.relname,
		        EXISTS (SELECT 1 FROM pg_catalog.pg_constraint k
		                 WHERE k.conrelid OPERATOR(pg_catalog.=) h.inhrelid
		                   AND k.conparentid OPERATOR(pg_catalog.=) $2)
		   FROM pg_catalog.pg_inherits h
		   JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) h.inhrelid
		   JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		  WHERE h.inhparent OPERATOR(pg_catalog.=) $1
		  ORDER BY h.inhrelid`

// requireAdoptedEverywhere proves that every direct partition of the parent
// carries a constraint adopted by the parent's existing constraint, and
// returns how many partitions there are. The server adopts or creates one on
// every partition when the parent's constraint is added and on every
// partition attached later, so a partition without one is a state the build
// does not understand.
func requireAdoptedEverywhere(ctx context.Context, q querier, parent partitionNode, existing parentConstraint) (int, error) {
	rows, err := q.Query(ctx, unadoptedPartitionsSQL, parent.oid, existing.oid)
	if err != nil {
		return 0, fmt.Errorf("read the partitions of %s adopted by constraint %s: %w", parent.qualified(), existing.name, err)
	}
	defer rows.Close()
	var (
		partitions int
		missing    []string
	)
	for rows.Next() {
		var (
			schema, name string
			adopted      bool
		)
		if err := rows.Scan(&schema, &name, &adopted); err != nil {
			return 0, fmt.Errorf("scan the partitions of %s: %w", parent.qualified(), err)
		}
		partitions++
		if !adopted {
			missing = append(missing, qualifiedName(schema, name))
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("read the partitions of %s adopted by constraint %s: %w", parent.qualified(), existing.name, err)
	}
	if len(missing) > 0 {
		return 0, fmt.Errorf("%w: constraint %s on %s exists, but partitions %s carry no constraint it adopted",
			ErrInvariantViolation, existing.name, parent.qualified(), strings.Join(missing, ", "))
	}
	return partitions, nil
}
