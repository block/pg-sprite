package executor

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// What the attach step requires before it runs the ATTACH: the proofs that
// let the server skip its two scans, and on the table, everything else the
// ATTACH would otherwise build or validate under its ACCESS EXCLUSIVE locks.
// For each index of the parent the ATTACH builds an index unless the table
// has one it takes (AttachPartitionEnsureIndexes); for each foreign key of
// the parent it creates and validates a foreign key unless the table has one
// it adopts (CloneFkReferencing). Every read runs under catalogReadPath, and
// every relation, function, operator, and cast is pg_catalog-qualified
// (CO-9).

// requireAttachReady requires the DEFAULT partition of the parent to be the
// one the run proved, or none, every proof to be a validated CHECK, and the
// table to hold, for every index and foreign key of the parent, one the
// ATTACH takes as it stands. When oids is set, it records each proof's
// constraint OID by its table. It returns the table facts it read.
func requireAttachReady(ctx context.Context, q querier, t attachPartitionTarget, oids map[uint32]uint32) (attachTableFacts, error) {
	var defaultOID uint32
	err := q.QueryRow(ctx, attachDefaultPartitionSQL, t.parentOID).Scan(&defaultOID)
	if errors.Is(err, pgx.ErrNoRows) {
		return attachTableFacts{}, fmt.Errorf("%w: ST-7: table %d is no longer partitioned", ErrInvariantViolation, t.parentOID)
	}
	if err != nil {
		return attachTableFacts{}, fmt.Errorf("read the DEFAULT partition of table %d: %w", t.parentOID, err)
	}
	if defaultOID != t.deflt.tableOID {
		return attachTableFacts{}, fmt.Errorf("%w: the parent's DEFAULT partition is table %d, and the run proved table %d",
			errAttachDefaultPartitionChanged, defaultOID, t.deflt.tableOID)
	}
	for _, p := range t.proofs() {
		state, oid, err := readAttachProof(ctx, q, p)
		if err != nil {
			return attachTableFacts{}, err
		}
		if state != attachProofDone {
			return attachTableFacts{}, fmt.Errorf("%w: %s has no validated CHECK %s", errAttachProofNotReady, p.qualified(), p.name())
		}
		if oids != nil {
			oids[p.tableOID] = oid
		}
	}
	facts, err := readAttachTableFacts(ctx, q, t)
	if err != nil {
		return attachTableFacts{}, err
	}
	if err := facts.requireReady(t.bound.qualified()); err != nil {
		return attachTableFacts{}, err
	}
	return facts, nil
}

// attachDefaultPartitionSQL reads the parent's DEFAULT partition, 0 for
// none. Every relation and operator is pg_catalog-qualified (CO-9).
const attachDefaultPartitionSQL = `SELECT p.partdefid
	FROM pg_catalog.pg_partitioned_table p
	WHERE p.partrelid OPERATOR(pg_catalog.=) $1`

// attachTableFacts is one read of the parent's indexes and foreign keys and
// the table's.
type attachTableFacts struct {
	indexes   attachIndexState
	parentFKs []catalogFK
	tableFKs  []catalogFK
	// tableKeys are, for each foreign key of the parent, its columns as the
	// table numbers them.
	tableKeys [][]int16
}

// readAttachTableFacts reads the indexes the attach's pairing turns on
// (readAttachIndexState, which proves the table still standalone), both
// tables' foreign keys, and each of the parent's foreign key columns by the
// table's numbering, matched by name as the attach matches columns.
func readAttachTableFacts(ctx context.Context, q querier, t attachPartitionTarget) (attachTableFacts, error) {
	var f attachTableFacts
	table := standaloneTable(t.bound.tableOID, t.bound.schema, t.bound.table)
	var err error
	if f.indexes, err = readAttachIndexState(ctx, q, t.parentOID, table); err != nil {
		return f, err
	}
	parentName := "table " + strconv.FormatUint(uint64(t.parentOID), 10)
	if f.parentFKs, err = readForeignKeys(ctx, q, t.parentOID, parentName, f.indexes.serverMajor); err != nil {
		return f, err
	}
	if f.tableFKs, err = readForeignKeys(ctx, q, table.oid, table.qualified(), f.indexes.serverMajor); err != nil {
		return f, err
	}
	for _, fk := range f.parentFKs {
		names, err := attachColumnNames(ctx, q, t.parentOID, parentName, fk.key)
		if err != nil {
			return f, err
		}
		attnums, err := fkColumnAttnums(ctx, q, table.oid, table.qualified(), names)
		if err != nil {
			return f, err
		}
		f.tableKeys = append(f.tableKeys, attnums)
	}
	return f, nil
}

const attachColumnNameSQL = `SELECT a.attname::pg_catalog.text
	FROM pg_catalog.pg_attribute a
	WHERE a.attrelid OPERATOR(pg_catalog.=) $1
	  AND a.attnum OPERATOR(pg_catalog.=) $2
	  AND NOT a.attisdropped`

// attachColumnNames reads the names of a table's columns by attribute
// number, in the order given.
func attachColumnNames(ctx context.Context, q querier, relOID uint32, relName string, attnums []int16) ([]string, error) {
	names := make([]string, 0, len(attnums))
	for _, attnum := range attnums {
		var name string
		if err := q.QueryRow(ctx, attachColumnNameSQL, relOID, attnum).Scan(&name); err != nil {
			return nil, fmt.Errorf("read column %d of %s: %w", attnum, relName, err)
		}
		names = append(names, name)
	}
	return names, nil
}

// requireReady refuses a table the ATTACH would give an index or a foreign
// key under its locks.
func (f attachTableFacts) requireReady(table string) error {
	for i, oid := range pairAttachIndexes(f.indexes) {
		if oid == 0 {
			return fmt.Errorf("%w: %s has no index the attach takes for the parent's index %s",
				errAttachIndexNotReady, table, f.indexes.parent[i].facts.name)
		}
	}
	for i, oid := range pairAttachForeignKeys(f.parentFKs, f.tableKeys, f.tableFKs) {
		if oid == 0 {
			return fmt.Errorf("%w: %s has no validated foreign key the attach adopts for the parent's foreign key %s",
				errAttachForeignKeyNotReady, table, f.parentFKs[i].name)
		}
	}
	return nil
}

// pairAttachForeignKeys returns, for each foreign key of the parent in
// order, the OID of the table's foreign key the attach adopts for it, or 0
// when the attach would create and validate one. The attach adopts a foreign
// key of the same definition (catalogFK.isRequested) attached to no other
// parent's, one per foreign key of the parent. The table's foreign key must
// also be validated: before PostgreSQL 18 the attach adopts only a
// validated one, and from 18 it validates a NOT VALID one under its locks.
// The order the server tries the table's foreign keys in is not the OID
// order, so a NOT VALID one of the definition among them is 0 even beside a
// validated one.
func pairAttachForeignKeys(parent []catalogFK, keys [][]int16, table []catalogFK) []uint32 {
	paired := make([]uint32, len(parent))
	taken := map[uint32]bool{}
	for i, p := range parent {
		spec := partitionedFKSpec{match: p.match, onUpdate: p.onUpdate, onDelete: p.onDelete,
			deferrable: p.deferrable, initiallyDeferred: p.initiallyDeferred}
		ref := fkReference{oid: p.refOID, attnums: p.refKey}
		var pick uint32
		for _, k := range table {
			if !attachMayAdopt(k, taken, spec, keys[i], ref) {
				continue
			}
			if !k.validated {
				pick = 0
				break
			}
			if pick == 0 {
				pick = k.oid
			}
		}
		if pick != 0 {
			paired[i] = pick
			taken[pick] = true
		}
	}
	return paired
}

// attachMayAdopt reports whether the attach may adopt the table's foreign
// key k for a foreign key of the parent: k is not adopted for another of the
// parent's, is attached to no parent's, and has the same definition, its
// columns numbered as the table numbers them (key).
func attachMayAdopt(k catalogFK, taken map[uint32]bool, spec partitionedFKSpec, key []int16, ref fkReference) bool {
	if taken[k.oid] {
		return false
	}
	if k.parentOID != 0 {
		return false
	}
	return k.isRequested(spec, key, ref)
}

// attachForeignKeyLockedTables lists the tables the ATTACH takes SHARE ROW
// EXCLUSIVE on through foreign keys, with the writes that lock conflicts
// with: each table a foreign key of the parent references, with its
// partitions when it is partitioned (CloneFkReferencing), and each table
// whose own foreign key references the parent (CloneFkReferenced). The
// ATTACH requests them while it holds ACCESS EXCLUSIVE on the table and the
// DEFAULT partition, so their writers are waited out before it runs.
func attachForeignKeyLockedTables(ctx context.Context, q querier, parentFKs []catalogFK, parentOID uint32) ([]lockedTable, error) {
	var oids []uint32
	for _, fk := range parentFKs {
		oids = append(oids, fk.refOID)
	}
	rows, err := q.Query(ctx, attachReferencingTablesSQL, parentOID)
	if err != nil {
		return nil, fmt.Errorf("list the tables referencing table %d: %w", parentOID, err)
	}
	referencing, err := pgx.CollectRows(rows, pgx.RowTo[uint32])
	if err != nil {
		return nil, fmt.Errorf("list the tables referencing table %d: %w", parentOID, err)
	}
	oids = append(oids, referencing...)
	seen := map[uint32]bool{}
	var tables []lockedTable
	for _, oid := range oids {
		if seen[oid] {
			continue
		}
		seen[oid] = true
		rel, found, err := relationByOID(ctx, q, oid)
		if err != nil {
			return nil, err
		}
		if !found {
			// Dropped since the foreign key was read: the ATTACH no
			// longer locks it.
			continue
		}
		tables = append(tables, lockedTable{schema: rel.schema, table: rel.name, descendants: rel.relkind == "p", modes: writeLockModes()})
	}
	return tables, nil
}

// attachReferencingTablesSQL lists the tables whose own foreign keys
// reference the table, leaving out the copies a partitioned referencing
// table's partitions carry, which the ATTACH does not lock.
const attachReferencingTablesSQL = `SELECT DISTINCT k.conrelid
	FROM pg_catalog.pg_constraint k
	WHERE k.confrelid OPERATOR(pg_catalog.=) $1
	  AND k.contype::pg_catalog.text OPERATOR(pg_catalog.=) 'f'
	  AND k.conparentid OPERATOR(pg_catalog.=) 0::pg_catalog.oid
	ORDER BY k.conrelid`
