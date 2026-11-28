package executor

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The foreign key step of an online ATTACH PARTITION. An ATTACH gives the
// attached table every enforced foreign key of the partitioned parent: it
// adopts a validated foreign key of the same definition the table already
// has, and otherwise creates one and validates it while it holds ACCESS
// EXCLUSIVE on the table, so the attach lasts as long as that scan. This
// holds for a parent's foreign key that is NOT VALID too, from PostgreSQL
// 18: the attach still validates the new partition. This file gives the
// table each of them first, while writes go on, through the partitioned
// foreign key build's leaf step (ensureLeafForeignKey): ADD FOREIGN KEY ...
// NOT VALID, then VALIDATE CONSTRAINT. The attach that follows adopts them
// and scans nothing for them. A NOT ENFORCED foreign key of the parent
// (PostgreSQL 18) needs nothing: the attach clones it without a scan.

// errAttachForeignKeyUnsupported is returned for a foreign key of the parent
// the leaf step cannot render: one whose ON DELETE names a column list.
var errAttachForeignKeyUnsupported = errors.New("the step cannot give the table this foreign key of the partitioned parent")

// attachForeignKeyResult is the table's foreign key for one foreign key of
// the parent.
type attachForeignKeyResult struct {
	parentFK string
	leafFKResult
}

// parentForeignKey is one foreign key of the partitioned parent: the
// definition the leaf step renders, by name, and the foreign key as the
// parent's pg_constraint records it, by number and OID.
type parentForeignKey struct {
	spec    partitionedFKSpec
	catalog catalogFK
}

// ensureAttachForeignKeys gives the table every enforced foreign key of the
// parent, validated, and returns what it did for each, in the parent foreign
// keys' OID order. Before anything is added it refuses, typed, a parent with
// any foreign key the step cannot give the table: one whose ON DELETE names
// a column list, two of one definition (the attach adopts the table's
// foreign key for one of them only), and one whose referenced table is
// partitioned or no longer the one the parent's foreign key references.
// Each foreign key is then the leaf step's work, which reads and decides
// every call, so a rerun resumes, and refuses, typed, a derived name held by
// another constraint and two foreign keys of the definition on the table. A
// failure returns the results of the foreign keys before it, which stay on
// the table. The table is a standalone node, not yet a partition, and every
// statement proves it still is. Each VALIDATE is bounded by b. The caller
// holds the parent's advisory table lock (LK-1).
func ensureAttachForeignKeys(ctx context.Context, pool *pgxpool.Pool, parentOID uint32, table partitionNode, b ValidateBudget,
	started leafFKStatementStarted) ([]attachForeignKeyResult, error) {
	if !table.standalone || table.relkind != "r" {
		return nil, fmt.Errorf("%w: the attach foreign key step takes a standalone plain table, got %s of kind %q",
			ErrInvariantViolation, table.qualified(), table.relkind)
	}
	if err := b.validate(); err != nil {
		return nil, err
	}
	tx := briefTx{pool: pool}
	var (
		major int
		fks   []parentForeignKey
	)
	err := tx.read(ctx, "list the foreign keys of the partitioned parent of "+table.qualified(), func(ctx context.Context, q querier) error {
		var versionNum int
		if err := q.QueryRow(ctx, `SELECT pg_catalog.current_setting('server_version_num')::pg_catalog.int4`).Scan(&versionNum); err != nil {
			return fmt.Errorf("read server_version_num: %w", err)
		}
		major = versionNum / 10000
		var err error
		fks, err = readParentForeignKeys(ctx, q, parentOID, major)
		return err
	})
	if err != nil {
		return nil, err
	}
	enforced := slices.DeleteFunc(fks, func(pfk parentForeignKey) bool { return pfk.catalog.notEnforced })
	if err := refuseParentForeignKeys(enforced); err != nil {
		return nil, fmt.Errorf("give %s the foreign keys of its partitioned parent: %w", table.qualified(), err)
	}
	refs := make([]fkReference, len(enforced))
	for i, pfk := range enforced {
		if refs[i], err = resolveParentFKReference(ctx, tx, pfk); err != nil {
			return nil, fmt.Errorf("give %s the foreign key %s: %w", table.qualified(), pfk.spec.name, err)
		}
	}
	results := make([]attachForeignKeyResult, 0, len(enforced))
	for i, pfk := range enforced {
		res, err := ensureLeafForeignKey(ctx, pool, resolvedFK{spec: pfk.spec, ref: refs[i], serverMajor: major}, table, b, started)
		if err != nil {
			return results, fmt.Errorf("give %s the foreign key %s: %w", table.qualified(), pfk.spec.name, err)
		}
		results = append(results, attachForeignKeyResult{parentFK: pfk.spec.name, leafFKResult: res})
	}
	return results, nil
}

// refuseParentForeignKeys refuses the enforced foreign keys of the parent
// the step cannot give the table, from the parent's catalog alone: one whose
// ON DELETE names a column list, which the leaf step does not render, and a
// second of one definition. The attach adopts a table's foreign key for one
// parent foreign key only and creates and validates its own for the other,
// and the leaf step cannot give the table two of one definition.
func refuseParentForeignKeys(fks []parentForeignKey) error {
	for i, pfk := range fks {
		if pfk.catalog.deleteSetColumns {
			return fmt.Errorf("%w: the ON DELETE of %s names a column list", errAttachForeignKeyUnsupported, pfk.spec.name)
		}
		for _, earlier := range fks[:i] {
			if sameParentFKDefinition(earlier.catalog, pfk.catalog) {
				return fmt.Errorf("%w: the partitioned parent's foreign keys %s and %s have one definition, and the attach adopts the table's foreign key for one of them only",
					ErrPartitionedForeignKeyConflict, earlier.spec.name, pfk.spec.name)
			}
		}
	}
	return nil
}

// sameParentFKDefinition reports whether two foreign keys of one table have
// the definition the attach compares before it adopts a partition's foreign
// key: the referenced table, the columns on both sides in order, MATCH, the
// actions, and deferrability. Whether either is validated is not compared:
// the attach adopts only a validated foreign key, whatever the parent's is.
func sameParentFKDefinition(a, b catalogFK) bool {
	return a.refOID == b.refOID &&
		slices.Equal(a.key, b.key) &&
		slices.Equal(a.refKey, b.refKey) &&
		a.match == b.match &&
		a.onUpdate == b.onUpdate &&
		a.onDelete == b.onDelete &&
		a.deferrable == b.deferrable &&
		a.initiallyDeferred == b.initiallyDeferred
}

// resolveParentFKReference resolves the referenced table of the parent's
// foreign key by name, as the leaf step renders and proves it, and refuses
// a partitioned one.
func resolveParentFKReference(ctx context.Context, tx briefTx, pfk parentForeignKey) (fkReference, error) {
	var ref fkReference
	err := tx.read(ctx, "resolve the table the foreign key "+pfk.spec.name+" references", func(ctx context.Context, q querier) error {
		var err error
		ref, err = resolveFKReference(ctx, q, pfk.spec)
		return err
	})
	if err != nil {
		return fkReference{}, err
	}
	return ref, proveParentFKReference(pfk, ref)
}

// proveParentFKReference refuses a referenced table whose name now resolves
// to a table other than the one the parent's foreign key references. The
// attach would not adopt a foreign key to it and would create and validate
// its own. A rerun reads the parent's foreign keys and resolves the name
// again.
func proveParentFKReference(pfk parentForeignKey, ref fkReference) error {
	// INV: ST-7 — the table is given a foreign key to the table the
	// parent's references, by identity, not to whatever holds its name.
	if ref.oid != pfk.catalog.refOID {
		return fmt.Errorf("%w: %s, the table the foreign key %s references, is now the table with OID %d, not %d",
			ErrInvariantViolation, qualifiedName(pfk.spec.refSchema, pfk.spec.refTable), pfk.spec.name, ref.oid, pfk.catalog.refOID)
	}
	return nil
}

// readParentForeignKeys reads the foreign keys of the parent through the
// leaf step's catalog read, in OID order, and names their columns and
// referenced table. A foreign key to a partitioned table puts on the
// referencing table one more foreign key per referenced partition, each a
// child of the table's own; those are left out, as the attach clones them
// from the one they belong to. A foreign key a sub-partitioned parent
// inherited is a child of its parent's, on another table, and stays.
func readParentForeignKeys(ctx context.Context, q querier, parentOID uint32, serverMajor int) ([]parentForeignKey, error) {
	parentName := fmt.Sprintf("table %d", parentOID)
	all, err := readForeignKeys(ctx, q, parentOID, parentName, serverMajor)
	if err != nil {
		return nil, err
	}
	own := make(map[uint32]bool, len(all))
	for _, k := range all {
		own[k.oid] = true
	}
	var fks []parentForeignKey
	for _, k := range all {
		if own[k.parentOID] {
			continue
		}
		spec, err := parentFKSpec(ctx, q, parentOID, parentName, k)
		if err != nil {
			return nil, err
		}
		fks = append(fks, parentForeignKey{spec: spec, catalog: k})
	}
	return fks, nil
}

// parentFKSpec names the parent's foreign key k as the leaf step renders
// it: its columns, its referenced table and columns, and its rules.
func parentFKSpec(ctx context.Context, q querier, parentOID uint32, parentName string, k catalogFK) (partitionedFKSpec, error) {
	columns, err := fkColumnNames(ctx, q, parentOID, parentName, k.key)
	if err != nil {
		return partitionedFKSpec{}, err
	}
	ref, found, err := relationByOID(ctx, q, k.refOID)
	if err != nil {
		return partitionedFKSpec{}, err
	}
	if !found {
		return partitionedFKSpec{}, fmt.Errorf("resolve the table with OID %d that foreign key %s references: %w", k.refOID, k.name, ErrTableNotFound)
	}
	refColumns, err := fkColumnNames(ctx, q, k.refOID, qualifiedName(ref.schema, ref.name), k.refKey)
	if err != nil {
		return partitionedFKSpec{}, err
	}
	return partitionedFKSpec{
		name:              k.name,
		columns:           columns,
		refSchema:         ref.schema,
		refTable:          ref.name,
		refColumns:        refColumns,
		match:             k.match,
		onUpdate:          k.onUpdate,
		onDelete:          k.onDelete,
		deferrable:        k.deferrable,
		initiallyDeferred: k.initiallyDeferred,
	}, nil
}

const fkColumnNameSQL = `SELECT a.attname::pg_catalog.text
		   FROM pg_catalog.pg_attribute a
		  WHERE a.attrelid OPERATOR(pg_catalog.=) $1
		    AND a.attnum OPERATOR(pg_catalog.=) $2
		    AND NOT a.attisdropped`

// fkColumnNames reads the names of a table's columns numbered attnums, in
// the order given.
func fkColumnNames(ctx context.Context, q querier, relOID uint32, relName string, attnums []int16) ([]string, error) {
	names := make([]string, 0, len(attnums))
	for _, attnum := range attnums {
		var name string
		err := q.QueryRow(ctx, fkColumnNameSQL, relOID, attnum).Scan(&name)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%s has no column numbered %d for the foreign key", relName, attnum)
		}
		if err != nil {
			return nil, fmt.Errorf("read column %d of %s: %w", attnum, relName, err)
		}
		names = append(names, name)
	}
	return names, nil
}
