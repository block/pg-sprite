package executor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// The catalog reads and the resume decision of the leaf key step. Every read
// runs under catalogReadPath, and every relation, function, operator, and
// cast is pg_catalog-qualified (CO-9). The step keeps no checkpoint: each call
// reads the leaf and decides from what it finds.

// leafKeyColumn is one key column as the leaf partition carries it.
type leafKeyColumn struct {
	name    string
	attnum  int16
	notNull bool
	// quoted is the name as the server's quote_ident prints it, which is how
	// pg_get_indexdef prints a key column.
	quoted string
}

const leafKeyColumnSQL = `SELECT a.attnum, a.attnotnull,
		        EXISTS (
		          SELECT 1 FROM pg_catalog.pg_constraint k
		           WHERE k.conrelid OPERATOR(pg_catalog.=) a.attrelid
		             AND k.contype::pg_catalog.text OPERATOR(pg_catalog.=) 'n'
		             AND k.conkey[1] OPERATOR(pg_catalog.=) a.attnum
		             AND NOT k.convalidated),
		        pg_catalog.quote_ident(a.attname)
		   FROM pg_catalog.pg_attribute a
		  WHERE a.attrelid OPERATOR(pg_catalog.=) $1
		    AND a.attname OPERATOR(pg_catalog.=) $2
		    AND a.attnum OPERATOR(pg_catalog.>) 0
		    AND NOT a.attisdropped`

// readLeafKeyColumns reads the requested key columns of the leaf, in key
// order. A column refuses NULL when pg_attribute.attnotnull is set and, from
// PostgreSQL 18, which catalogs the NOT NULL as a constraint, that constraint
// is validated: attnotnull is also set for a NOT VALID not-null constraint,
// under which the column can still hold NULLs. Before 18 the catalog has no
// not-null constraints, so attnotnull alone decides.
func readLeafKeyColumns(ctx context.Context, q querier, node partitionNode, names []string) ([]leafKeyColumn, error) {
	columns := make([]leafKeyColumn, 0, len(names))
	for _, name := range names {
		c := leafKeyColumn{name: name}
		var attNotNull, notNullNotValid bool
		err := q.QueryRow(ctx, leafKeyColumnSQL, node.oid, name).Scan(&c.attnum, &attNotNull, &notNullNotValid, &c.quoted)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("table %s has no column %q for the key", node.qualified(), name)
		}
		if err != nil {
			return nil, fmt.Errorf("read key column %q of partition %s: %w", name, node.qualified(), err)
		}
		c.notNull = attNotNull && !notNullNotValid
		columns = append(columns, c)
	}
	return columns, nil
}

// leafKeyDefinitionKey is the definitionKey of the index the step builds: a
// plain unique btree index on exactly the key columns, in order, with the
// columns' default operator classes and collations, no INCLUDE, no predicate,
// and no storage parameters. pg_get_indexdef prints such an index as
// "USING btree (a, b)", each column as quote_ident prints it, and leaves out
// everything at its default, so an index with any other definition prints
// differently and does not compare equal.
func leafKeyDefinitionKey(columns []leafKeyColumn) string {
	quoted := make([]string, 0, len(columns))
	for _, c := range columns {
		quoted = append(quoted, c.quoted)
	}
	return "unique=true USING btree (" + strings.Join(quoted, ", ") + ")"
}

// leafConstraint is one PRIMARY KEY or UNIQUE constraint of the leaf, with
// the index that backs it.
type leafConstraint struct {
	oid     uint32
	name    string
	contype string
	// parentOID is pg_constraint.conparentid: nonzero once a partitioned
	// table's constraint has adopted this one.
	parentOID uint32
	// deferrable is pg_constraint.condeferrable. The index definition does
	// not show it, and a parent's immediate constraint still adopts a
	// deferrable one.
	deferrable bool
	key        []int16
	index      relationFacts
}

const leafConstraintsSQL = `SELECT k.oid, k.conname, k.contype::pg_catalog.text, k.conindid, k.conparentid, k.condeferrable, k.conkey
		   FROM pg_catalog.pg_constraint k
		  WHERE k.conrelid OPERATOR(pg_catalog.=) $1
		    AND k.contype::pg_catalog.text OPERATOR(pg_catalog.=) ANY (ARRAY['p', 'u']::pg_catalog.text[])
		  ORDER BY k.oid`

// readLeafConstraints lists the leaf's PRIMARY KEY and UNIQUE constraints and
// reads the index behind each.
func readLeafConstraints(ctx context.Context, q querier, node partitionNode) ([]leafConstraint, error) {
	rows, err := q.Query(ctx, leafConstraintsSQL, node.oid)
	if err != nil {
		return nil, fmt.Errorf("list the key constraints of partition %s: %w", node.qualified(), err)
	}
	var (
		found    []leafConstraint
		indexOID []uint32
	)
	for rows.Next() {
		var c leafConstraint
		var conindid uint32
		if err := rows.Scan(&c.oid, &c.name, &c.contype, &conindid, &c.parentOID, &c.deferrable, &c.key); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan the key constraints of partition %s: %w", node.qualified(), err)
		}
		found = append(found, c)
		indexOID = append(indexOID, conindid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the key constraints of partition %s: %w", node.qualified(), err)
	}
	for i := range found {
		idx, ok, err := relationByOID(ctx, q, indexOID[i])
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%w: index %d of constraint %s on partition %s is not in the catalog",
				ErrInvariantViolation, indexOID[i], found[i].name, node.qualified())
		}
		found[i].index = idx
	}
	return found, nil
}

const constraintNamedSQL = `SELECT EXISTS (
		   SELECT 1 FROM pg_catalog.pg_constraint k
		    WHERE k.conrelid OPERATOR(pg_catalog.=) $1
		      AND k.conname OPERATOR(pg_catalog.=) $2)`

// leafKeyFacts is everything the resume decision reads about the leaf, in one
// read-only transaction. The transaction is READ COMMITTED, so each read takes
// its own snapshot: the caller's advisory lock on the parent keeps other runs
// of the step out between them, and the constraint step proves again, under
// the leaf's lock, everything it acts on.
type leafKeyFacts struct {
	columns     []leafKeyColumn
	constraints []leafConstraint
	// occupant is the relation under the derived name, when occupied.
	occupant relationFacts
	occupied bool
	// nameHeldByConstraint is whether any constraint of the leaf, of any
	// kind, carries the derived name.
	nameHeldByConstraint bool
	// reusable is every valid index of the leaf attached to nothing and
	// backing no constraint, whatever its name.
	reusable []relationFacts
	// quarantine is the droppable debris an earlier recovery renamed and
	// did not drop; the concurrent build refuses a table that carries it.
	quarantine []invalidIndex
}

// readLeafKeyFacts reads the leaf's key columns, key constraints, the
// relation under the derived name, its unattached indexes, and its
// quarantined debris.
func readLeafKeyFacts(ctx context.Context, q querier, node partitionNode, spec partitionedKeySpec, name string) (leafKeyFacts, error) {
	var facts leafKeyFacts
	// A partition renamed, detached, or dropped since it was listed is not
	// the leaf to read (INV: ST-7).
	if err := proveNodeIn(ctx, q, node); err != nil {
		return facts, err
	}
	var err error
	if facts.columns, err = readLeafKeyColumns(ctx, q, node, spec.columns); err != nil {
		return facts, err
	}
	if facts.constraints, err = readLeafConstraints(ctx, q, node); err != nil {
		return facts, err
	}
	if facts.occupant, facts.occupied, err = relationByName(ctx, q, node.schema, name); err != nil {
		return facts, err
	}
	if err := q.QueryRow(ctx, constraintNamedSQL, node.oid, name).Scan(&facts.nameHeldByConstraint); err != nil {
		return facts, fmt.Errorf("look for constraint %s on partition %s: %w", name, node.qualified(), err)
	}
	if facts.reusable, err = reusableIndexes(ctx, q, node.oid); err != nil {
		return facts, err
	}
	facts.quarantine, err = listDroppableQuarantinedIndexes(ctx, q, indexTarget{tableOID: node.oid, schema: node.schema, table: node.name})
	return facts, err
}

// leafKeyState is where the resume decision found the leaf.
type leafKeyState int

const (
	// leafKeyDone: the leaf has the requested constraint, attached to no
	// parent constraint, backed by a valid index of the key's definition.
	leafKeyDone leafKeyState = iota + 1
	// leafKeyConstraintPending: a valid unique index of the key's definition
	// waits for the constraint.
	leafKeyConstraintPending
	// leafKeyBuildNeeded: no usable index exists, so one is built, after
	// any debris of an earlier build is removed.
	leafKeyBuildNeeded
)

// leafKeyPlan is the resume decision: the state, and the constraint or index
// it found.
type leafKeyPlan struct {
	state      leafKeyState
	constraint leafConstraint
	index      relationFacts
	// debris is whether the build has to remove an earlier build's leftover
	// first: an invalid index of the key's definition under the derived
	// name, or quarantined debris.
	debris bool
}

// decideLeafKey decides, from the facts readLeafKeyFacts read, which step
// brings the leaf to the requested key. Existing constraints are judged
// first, so a matching one is reused whatever its name; then the primary
// key's columns; then the derived name; then the leaf's unattached indexes.
func decideLeafKey(spec partitionedKeySpec, node partitionNode, name string, facts leafKeyFacts) (leafKeyPlan, error) {
	want := leafKeyDefinitionKey(facts.columns)
	match, err := matchingLeafConstraint(spec, node, facts, want)
	if err != nil {
		return leafKeyPlan{}, err
	}
	if match != nil {
		return leafKeyPlan{state: leafKeyDone, constraint: *match, index: match.index}, nil
	}
	if err := requireNotNullForPrimaryKey(spec, node, facts.columns); err != nil {
		return leafKeyPlan{}, err
	}
	if facts.nameHeldByConstraint {
		return leafKeyPlan{}, fmt.Errorf("%w: partition %s already has a constraint named %s that is not the requested key",
			ErrPartitionedKeyNameTaken, node.qualified(), name)
	}
	if facts.occupied {
		return judgeLeafKeyOccupant(node, facts.occupant, want)
	}
	for _, candidate := range facts.reusable {
		key, err := candidate.definitionKey()
		if err != nil {
			return leafKeyPlan{}, err
		}
		if key == want {
			return leafKeyPlan{state: leafKeyConstraintPending, index: candidate}, nil
		}
	}
	return leafKeyPlan{state: leafKeyBuildNeeded, debris: len(facts.quarantine) > 0}, nil
}

// matchingLeafConstraint returns the leaf's constraint that already is the
// requested key, or nil, and refuses a leaf whose key constraints conflict
// with the request. A parent's constraint adopts a partition's constraint of
// the same index definition, so a second constraint on the same columns, of
// another kind or definition, or one already adopted elsewhere, would leave
// the adoption to chance; the step adds nothing then.
func matchingLeafConstraint(spec partitionedKeySpec, node partitionNode, facts leafKeyFacts, want string) (*leafConstraint, error) {
	var matches []leafConstraint
	for _, c := range facts.constraints {
		equal, err := c.isRequestedKey(spec, want)
		if err != nil {
			return nil, err
		}
		if err := conflictingLeafConstraint(spec, node, facts.columns, c, equal); err != nil {
			return nil, err
		}
		if equal {
			matches = append(matches, c)
		}
	}
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return &matches[0], nil
	default:
		names := make([]string, 0, len(matches))
		for _, c := range matches {
			names = append(names, c.name)
		}
		return nil, fmt.Errorf("%w: partition %s has %d constraints that each match the requested key: %s",
			ErrPartitionedKeyConflict, node.qualified(), len(matches), strings.Join(names, ", "))
	}
}

// isRequestedKey reports whether the constraint is of the requested kind, not
// deferrable, and backed by a valid index of the key's definition. The
// requested key is checked immediately; a deferrable constraint the parent
// adopted would check it only at commit, and could not be the arbiter of an
// INSERT ... ON CONFLICT routed to the leaf.
func (c leafConstraint) isRequestedKey(spec partitionedKeySpec, want string) (bool, error) {
	if c.contype != spec.kind.contype() || !c.index.valid {
		return false, nil
	}
	if c.deferrable {
		return false, nil
	}
	key, err := c.index.definitionKey()
	if err != nil {
		return false, err
	}
	return key == want, nil
}

// conflictingLeafConstraint refuses a constraint that stands in the way of the
// requested one. equal is whether the constraint is the requested key.
func conflictingLeafConstraint(spec partitionedKeySpec, node partitionNode, columns []leafKeyColumn, c leafConstraint, equal bool) error {
	switch {
	case equal && c.parentOID != 0:
		return fmt.Errorf("%w: constraint %s on partition %s is the requested key but is already attached to the constraint with OID %d of a parent table",
			ErrPartitionedKeyConflict, c.name, node.qualified(), c.parentOID)
	case !equal && sameKeyColumns(c.key, columns):
		return fmt.Errorf("%w: constraint %s on partition %s covers the key's columns but is not the requested %s of that definition",
			ErrPartitionedKeyConflict, c.name, node.qualified(), spec.kind.clause())
	case !equal && spec.kind == partitionedPrimaryKey && c.contype == partitionedPrimaryKey.contype():
		return fmt.Errorf("%w: partition %s already has primary key %s on other columns",
			ErrPartitionedKeyConflict, node.qualified(), c.name)
	}
	return nil
}

// sameKeyColumns reports whether a constraint's columns are the key's, in
// order.
func sameKeyColumns(conkey []int16, columns []leafKeyColumn) bool {
	attnums := make([]int16, 0, len(columns))
	for _, c := range columns {
		attnums = append(attnums, c.attnum)
	}
	return slices.Equal(conkey, attnums)
}

// requireNotNullForPrimaryKey refuses a primary key over a column the leaf
// allows to be NULL: the constraint step would set NOT NULL, which scans the
// partition under ACCESS EXCLUSIVE.
func requireNotNullForPrimaryKey(spec partitionedKeySpec, node partitionNode, columns []leafKeyColumn) error {
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
	return fmt.Errorf("%w: partition %s allows NULL in %s; make the columns NOT NULL first, validating any NOT VALID not-null constraint on them",
		ErrPartitionedKeyNullableColumn, node.qualified(), strings.Join(nullable, ", "))
}

// judgeLeafKeyOccupant decides what the relation under the derived name is: a
// valid index of the key's definition waiting for its constraint, an invalid
// one an earlier build left, or something the step will not take from its
// owner.
func judgeLeafKeyOccupant(node partitionNode, occ relationFacts, want string) (leafKeyPlan, error) {
	name := qualifiedName(occ.schema, occ.name)
	if occ.relkind != "i" || occ.tableOID != node.oid {
		return leafKeyPlan{}, fmt.Errorf("%w: %s is %s, not the key's index on %s",
			ErrPartitionedKeyNameTaken, name, describeRelation(occ), node.qualified())
	}
	if occ.isPartition {
		return leafKeyPlan{}, fmt.Errorf("%w: %s is attached to a partitioned index", ErrPartitionedKeyNameTaken, name)
	}
	if occ.backsConstraint {
		return leafKeyPlan{}, fmt.Errorf("%w: %s backs a constraint on partition %s that is not the requested key",
			ErrPartitionedKeyNameTaken, name, node.qualified())
	}
	key, err := occ.definitionKey()
	if err != nil {
		return leafKeyPlan{}, err
	}
	if key != want {
		return leafKeyPlan{}, fmt.Errorf("%w: %s on partition %s does not have the key's definition",
			ErrPartitionedKeyNameTaken, name, node.qualified())
	}
	if !occ.valid {
		return leafKeyPlan{state: leafKeyBuildNeeded, debris: true}, nil
	}
	return leafKeyPlan{state: leafKeyConstraintPending, index: occ}, nil
}
