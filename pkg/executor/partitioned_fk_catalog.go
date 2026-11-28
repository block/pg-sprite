package executor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// The catalog reads and the resume decision of the leaf foreign key step.
// Every read runs under catalogReadPath, and every relation, function,
// operator, and cast is pg_catalog-qualified (CO-9). The step keeps no
// checkpoint: each call reads the leaf and decides from what it finds.

// fkReference is the referenced table as the build resolved it, with the
// attribute numbers of the referenced columns in the foreign key's order.
type fkReference struct {
	oid     uint32
	schema  string
	table   string
	attnums []int16
}

// resolvedFK is the requested foreign key with what the build resolved for
// it: the referenced table, and the server major whose catalog it reads.
type resolvedFK struct {
	spec        partitionedFKSpec
	ref         fkReference
	serverMajor int
}

// resolveFKReference resolves the referenced table by name and its
// referenced columns by name. Only a plain table is taken: a partitioned
// referenced table makes the parent's statement clone the foreign key onto
// every referenced partition, which this build neither proves nor reads
// back.
func resolveFKReference(ctx context.Context, q querier, spec partitionedFKSpec) (fkReference, error) {
	name := qualifiedName(spec.refSchema, spec.refTable)
	rel, found, err := relationByName(ctx, q, spec.refSchema, spec.refTable)
	if err != nil {
		return fkReference{}, err
	}
	if !found {
		return fkReference{}, fmt.Errorf("resolve referenced table %s: %w", name, ErrTableNotFound)
	}
	if rel.relkind != "r" {
		return fkReference{}, fmt.Errorf("%w: %s is %s, not a plain table",
			ErrPartitionedForeignKeyReferencedTable, name, describeRelation(rel))
	}
	attnums, err := fkColumnAttnums(ctx, q, rel.oid, name, spec.refColumns)
	if err != nil {
		return fkReference{}, err
	}
	return fkReference{oid: rel.oid, schema: rel.schema, table: rel.name, attnums: attnums}, nil
}

const fkColumnSQL = `SELECT a.attnum
		   FROM pg_catalog.pg_attribute a
		  WHERE a.attrelid OPERATOR(pg_catalog.=) $1
		    AND a.attname OPERATOR(pg_catalog.=) $2
		    AND a.attnum OPERATOR(pg_catalog.>) 0
		    AND NOT a.attisdropped`

// fkColumnAttnums reads the attribute numbers of the named columns of a
// table, in the order given. A partition can number its columns differently
// from its parent, so each table's are read on their own.
func fkColumnAttnums(ctx context.Context, q querier, relOID uint32, relName string, names []string) ([]int16, error) {
	attnums := make([]int16, 0, len(names))
	for _, name := range names {
		var attnum int16
		err := q.QueryRow(ctx, fkColumnSQL, relOID, name).Scan(&attnum)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("table %s has no column %q for the foreign key", relName, name)
		}
		if err != nil {
			return nil, fmt.Errorf("read column %q of table %s: %w", name, relName, err)
		}
		attnums = append(attnums, attnum)
	}
	return attnums, nil
}

// catalogFK is one foreign key of a table as pg_constraint records it.
type catalogFK struct {
	oid    uint32
	name   string
	refOID uint32
	key    []int16
	refKey []int16
	// match, onUpdate, and onDelete are confmatchtype, confupdtype, and
	// confdeltype.
	match    string
	onUpdate string
	onDelete string
	// deleteSetColumns is the attribute numbers of the foreign key's ON
	// DELETE column list (pg_constraint.confdelsetcols, from version 15),
	// empty when there is none.
	deleteSetColumns []int16
	// notEnforced is whether the foreign key is NOT ENFORCED
	// (pg_constraint.conenforced false, from version 18).
	notEnforced       bool
	deferrable        bool
	initiallyDeferred bool
	validated         bool
	// parentOID is pg_constraint.conparentid: nonzero once a partitioned
	// table's foreign key has adopted this one.
	parentOID uint32
}

const foreignKeysSQLFormat = `SELECT k.oid, k.conname, k.confrelid, k.conkey, k.confkey,
		        k.confmatchtype::pg_catalog.text, k.confupdtype::pg_catalog.text, k.confdeltype::pg_catalog.text,
		        %s, %s,
		        k.condeferrable, k.condeferred, k.convalidated, k.conparentid
		   FROM pg_catalog.pg_constraint k
		  WHERE k.conrelid OPERATOR(pg_catalog.=) $1
		    AND k.contype::pg_catalog.text OPERATOR(pg_catalog.=) 'f'
		  ORDER BY k.oid`

// foreignKeysSQL lists a table's foreign keys. Version 14 has no
// confdelsetcols, and no ON DELETE column lists, so its query reads an
// empty list there; versions before 18 have no conenforced, and no NOT
// ENFORCED foreign keys, so their queries read false there.
func foreignKeysSQL(serverMajor int) string {
	deleteSetColumns := `COALESCE(k.confdelsetcols, '{}'::pg_catalog.int2[])`
	if serverMajor < deleteSetColumnsMajor {
		deleteSetColumns = `'{}'::pg_catalog.int2[]`
	}
	notEnforced := `NOT k.conenforced`
	if serverMajor < 18 {
		notEnforced = `false`
	}
	return fmt.Sprintf(foreignKeysSQLFormat, deleteSetColumns, notEnforced)
}

// readForeignKeys lists the foreign keys of the table relOID.
func readForeignKeys(ctx context.Context, q querier, relOID uint32, relName string, serverMajor int) ([]catalogFK, error) {
	rows, err := q.Query(ctx, foreignKeysSQL(serverMajor), relOID)
	if err != nil {
		return nil, fmt.Errorf("list the foreign keys of %s: %w", relName, err)
	}
	defer rows.Close()
	var fks []catalogFK
	for rows.Next() {
		var k catalogFK
		if err := rows.Scan(&k.oid, &k.name, &k.refOID, &k.key, &k.refKey,
			&k.match, &k.onUpdate, &k.onDelete, &k.deleteSetColumns, &k.notEnforced,
			&k.deferrable, &k.initiallyDeferred, &k.validated, &k.parentOID); err != nil {
			return nil, fmt.Errorf("scan the foreign keys of %s: %w", relName, err)
		}
		fks = append(fks, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the foreign keys of %s: %w", relName, err)
	}
	return fks, nil
}

// isRequested reports whether the foreign key has the requested
// definition on a table whose foreign key columns are attnums: the same
// definition apart from any ON DELETE column list (sameDefinition), and the
// requested column list, compared as a set. Whether it is validated or
// adopted is judged apart.
func (k catalogFK) isRequested(spec partitionedFKSpec, attnums []int16, ref fkReference) bool {
	return k.sameDefinition(spec, attnums, ref) && sameColumnSet(k.deleteSetColumns, spec.deleteSetAttnums(attnums))
}

// sameColumnSet reports whether two attribute number lists hold the same
// columns, in any order.
func sameColumnSet(a, b []int16) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// isAdoptable reports whether the parent's statement would adopt the
// foreign key for the requested one instead of creating and validating its
// own, by the rule PostgreSQL 14 to 17 apply, the majors the build runs on:
// a validated foreign key attached to no parent's foreign key, with the
// same definition apart from any ON DELETE column list (sameDefinition).
// That rule does not compare the column list, so it adopts a foreign key
// whose list differs from the parent's, and with it a delete rule the
// parent does not have. PostgreSQL 18 applies another rule, adopting a NOT
// VALID foreign key and erroring on one whose enforcement differs; the
// build refuses 18 and later.
func (k catalogFK) isAdoptable(spec partitionedFKSpec, attnums []int16, ref fkReference) bool {
	return k.sameDefinition(spec, attnums, ref) && k.adoptionCandidate()
}

// adoptionCandidate reports whether the foreign key is one PostgreSQL 14 to
// 17 consider adopting at all: validated, and attached to no parent's
// foreign key.
func (k catalogFK) adoptionCandidate() bool {
	return k.validated && k.parentOID == 0
}

// sameDefinition reports whether the foreign key has the same referenced
// table and columns, in order, and the same MATCH, actions, deferrability,
// and enforcement as the requested one. It does not look at the ON DELETE
// column list.
func (k catalogFK) sameDefinition(spec partitionedFKSpec, attnums []int16, ref fkReference) bool {
	return k.refOID == ref.oid &&
		slices.Equal(k.key, attnums) &&
		slices.Equal(k.refKey, ref.attnums) &&
		k.sameRules(spec)
}

// sameRules reports whether the foreign key's MATCH, actions,
// enforcement, and deferrability are the requested ones.
func (k catalogFK) sameRules(spec partitionedFKSpec) bool {
	return k.match == spec.match &&
		k.onUpdate == spec.onUpdate &&
		k.onDelete == spec.onDelete &&
		!k.notEnforced &&
		k.deferrable == spec.deferrable &&
		k.initiallyDeferred == spec.initiallyDeferred
}

// requestedForeignKeys returns the foreign keys that have the requested
// definition.
func requestedForeignKeys(fks []catalogFK, spec partitionedFKSpec, attnums []int16, ref fkReference) []catalogFK {
	var out []catalogFK
	for _, k := range fks {
		if k.isRequested(spec, attnums, ref) {
			out = append(out, k)
		}
	}
	return out
}

// adoptableForeignKeys returns the foreign keys the parent's statement
// could adopt for the requested one (isAdoptable).
func adoptableForeignKeys(fks []catalogFK, spec partitionedFKSpec, attnums []int16, ref fkReference) []catalogFK {
	var out []catalogFK
	for _, k := range fks {
		if k.isAdoptable(spec, attnums, ref) {
			out = append(out, k)
		}
	}
	return out
}

// leafFKFacts is everything the resume decision reads from one snapshot of
// the leaf.
type leafFKFacts struct {
	// attnums are the leaf's foreign key columns.
	attnums     []int16
	foreignKeys []catalogFK
	// nameHeld is whether a constraint of the leaf, of any kind, carries
	// the derived name.
	nameHeld bool
}

const fkConstraintNamedSQL = `SELECT EXISTS (
		   SELECT 1 FROM pg_catalog.pg_constraint k
		    WHERE k.conrelid OPERATOR(pg_catalog.=) $1
		      AND k.conname OPERATOR(pg_catalog.=) $2)`

// readLeafFKFacts proves the leaf is still in the tree and reads its
// foreign key columns, its foreign keys, and whether the derived name is
// taken.
func readLeafFKFacts(ctx context.Context, q querier, fk resolvedFK, node partitionNode, name string) (leafFKFacts, error) {
	var facts leafFKFacts
	// A partition renamed, detached, or dropped since it was listed is not
	// the leaf to read (INV: ST-7).
	if err := proveNodeIn(ctx, q, node); err != nil {
		return facts, err
	}
	var err error
	if facts.attnums, err = fkColumnAttnums(ctx, q, node.oid, node.qualified(), fk.spec.columns); err != nil {
		return facts, err
	}
	if facts.foreignKeys, err = readForeignKeys(ctx, q, node.oid, node.qualified(), fk.serverMajor); err != nil {
		return facts, err
	}
	if err := q.QueryRow(ctx, fkConstraintNamedSQL, node.oid, name).Scan(&facts.nameHeld); err != nil {
		return facts, fmt.Errorf("look for constraint %s on partition %s: %w", name, node.qualified(), err)
	}
	return facts, nil
}

// leafFKState is where the resume decision found the leaf.
type leafFKState int

const (
	// leafFKDone: the leaf has exactly one foreign key of the requested
	// definition, validated and adopted by nothing.
	leafFKDone leafFKState = iota + 1
	// leafFKValidatePending: that foreign key exists but is NOT VALID.
	leafFKValidatePending
	// leafFKAddNeeded: the leaf has no foreign key of the requested
	// definition, so the step adds one NOT VALID under the derived name.
	leafFKAddNeeded
)

// leafFKPlan is the resume decision: the state, and the foreign key it
// found.
type leafFKPlan struct {
	state leafFKState
	fk    catalogFK
}

// decideLeafForeignKey decides, from one snapshot of the leaf, which step
// brings it to the requested foreign key. A foreign key of the requested
// definition is reused whatever its name, so an earlier run's and an
// operator's own are both found; the derived name is judged only when there
// is none. Foreign keys of another definition are left alone: the parent's
// statement does not adopt them. One it would adopt (isAdoptable) that
// differs only in its ON DELETE column list is refused, because the server
// picks among the adoptable ones by name, so it could take that one in place
// of the step's.
func decideLeafForeignKey(node partitionNode, name string, fk resolvedFK, facts leafFKFacts) (leafFKPlan, error) {
	for _, k := range adoptableForeignKeys(facts.foreignKeys, fk.spec, facts.attnums, fk.ref) {
		if !k.isRequested(fk.spec, facts.attnums, fk.ref) {
			return leafFKPlan{}, fmt.Errorf("%w: foreign key %s on partition %s has an ON DELETE column list, which the server does not compare when it chooses the foreign key the parent's statement adopts, so the parent's could adopt it in place of the requested one",
				ErrPartitionedForeignKeyConflict, k.name, node.qualified())
		}
	}
	matches := requestedForeignKeys(facts.foreignKeys, fk.spec, facts.attnums, fk.ref)
	for _, k := range matches {
		if k.parentOID != 0 {
			return leafFKPlan{}, fmt.Errorf("%w: foreign key %s on partition %s has the requested definition but is already attached to the foreign key with OID %d of a parent table",
				ErrPartitionedForeignKeyConflict, k.name, node.qualified(), k.parentOID)
		}
	}
	if len(matches) > 1 {
		names := make([]string, 0, len(matches))
		for _, k := range matches {
			names = append(names, k.name)
		}
		return leafFKPlan{}, fmt.Errorf("%w: partition %s has %d foreign keys of the requested definition: %s",
			ErrPartitionedForeignKeyConflict, node.qualified(), len(matches), strings.Join(names, ", "))
	}
	if len(matches) == 1 {
		if matches[0].validated {
			return leafFKPlan{state: leafFKDone, fk: matches[0]}, nil
		}
		return leafFKPlan{state: leafFKValidatePending, fk: matches[0]}, nil
	}
	if facts.nameHeld {
		return leafFKPlan{}, fmt.Errorf("%w: partition %s already has a constraint named %s that is not the requested foreign key",
			ErrPartitionedForeignKeyNameTaken, node.qualified(), name)
	}
	return leafFKPlan{state: leafFKAddNeeded}, nil
}
