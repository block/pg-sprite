package executor

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
)

// How an ATTACH PARTITION pairs the table's indexes with the parent's, and
// the reads it pairs from. The server (AttachPartitionEnsureIndexes in
// src/backend/commands/tablecmds.c) walks the parent's indexes in OID order
// and, for each, takes the first of the table's indexes in OID order that is
// attached to nothing yet, valid, and of the same definition; when the
// parent's index backs a constraint, the table's index must back one too,
// from PostgreSQL 17 of the same type. A parent index nothing matches gets an
// index built under the attach's ACCESS EXCLUSIVE lock. The step simulates
// that pairing from catalog facts before it builds, to build only what the
// attach would otherwise build, and after, to prove the attach builds
// nothing. Every read runs under catalogReadPath, and every relation and
// operator is pg_catalog-qualified (CO-9).

// errAttachIndexIncomplete is returned when, after the step's builds, the
// attach would still build an index, or would pair the table's indexes other
// than the step reported, or the parent's indexes changed during the step. A
// rerun reads the catalog again and builds what is missing.
var errAttachIndexIncomplete = errors.New("the attach would still build an index on the table")

// sameConstraintTypeMajor is the first server major whose attach requires a
// constraint-backed parent index's table index to back a constraint of the
// same type; before it, a constraint of any index-owning type matches.
const sameConstraintTypeMajor = 17

// attachTableIndex is one index of the table as the attach's pairing sees
// it.
type attachTableIndex struct {
	oid  uint32
	name string
	key  string
	// contype is the type of the primary key, unique, or exclusion
	// constraint the index backs on the table, or empty.
	contype string
	// valid is pg_index.indisvalid.
	valid bool
	// attached is pg_class.relispartition: the index is attached to a
	// partitioned index already.
	attached bool
}

// attachIndexState is one read of what the pairing turns on: the server's
// major, the parent's indexes, and the table's.
type attachIndexState struct {
	serverMajor int
	parent      []parentIndex
	table       []attachTableIndex
}

// pairAttachIndexes returns, for each parent index in order, the OID of the
// table index the attach takes for it, or 0 when the attach would build one.
// A table index goes to one parent index at most.
func pairAttachIndexes(s attachIndexState) []uint32 {
	paired := make([]uint32, len(s.parent))
	taken := map[uint32]bool{}
	for i, p := range s.parent {
		for _, c := range s.table {
			if taken[c.oid] || !attachTakes(p, c, s.serverMajor) {
				continue
			}
			paired[i] = c.oid
			taken[c.oid] = true
			break
		}
	}
	return paired
}

// attachTakes reports whether the attach would pair the table index with the
// parent index, if both were still unpaired.
func attachTakes(p parentIndex, c attachTableIndex, serverMajor int) bool {
	if c.attached {
		return false
	}
	if !c.valid {
		return false
	}
	if c.key != p.key {
		return false
	}
	// The server compares exclusion operators, so an exclusion constraint's
	// index and any other index never match.
	if (c.contype == "x") != (p.contype == "x") {
		return false
	}
	if p.contype == "" {
		return true
	}
	if c.contype == "" {
		return false
	}
	if serverMajor < sameConstraintTypeMajor {
		return true
	}
	return c.contype == p.contype
}

// attachIndexesComplete proves, from a read after the builds, that the attach
// builds nothing: the parent has the indexes it had at the first read, every
// one of them pairs with a table index, and each pairs with the index the
// step reported for it.
func attachIndexesComplete(first []parentIndex, after attachIndexState, results []attachIndexResult) error {
	if !sameParentIndexes(first, after.parent) {
		return fmt.Errorf("%w: the partitioned parent's indexes changed during the step", errAttachIndexIncomplete)
	}
	if len(results) != len(after.parent) {
		return fmt.Errorf("%w: the step reported %d indexes for %d indexes of the parent",
			errAttachIndexIncomplete, len(results), len(after.parent))
	}
	for i, oid := range pairAttachIndexes(after) {
		if oid != results[i].indexOID {
			return fmt.Errorf("%w: the parent's index %s pairs with table index %d (0 when the attach builds one), not %s (%d)",
				errAttachIndexIncomplete, after.parent[i].facts.name, oid, results[i].index, results[i].indexOID)
		}
	}
	return nil
}

// sameParentIndexes reports whether two reads list the same parent indexes,
// by OID, definition, and constraint type, in the same order.
func sameParentIndexes(a, b []parentIndex) bool {
	return slices.EqualFunc(a, b, func(x, y parentIndex) bool {
		return x.facts.oid == y.facts.oid && x.key == y.key && x.contype == y.contype
	})
}

// readAttachIndexState proves the parent is still a partitioned table and
// the table still standalone, then reads the server's major and both index
// lists.
func readAttachIndexState(ctx context.Context, q querier, parentOID uint32, table partitionNode) (attachIndexState, error) {
	var s attachIndexState
	// A dropped parent reads as no kind at all, which the check below
	// refuses the same as a table of another kind.
	var relkind string
	err := q.QueryRow(ctx, relkindSQL, parentOID).Scan(&relkind)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return s, fmt.Errorf("read the kind of table %d: %w", parentOID, err)
	}
	// INV: ST-7 — the indexes read here are those of the partitioned table
	// the parent was resolved as.
	if relkind != "p" {
		return s, fmt.Errorf("%w: ST-7: table %d, the parent %s is to be attached to, is no longer a partitioned table",
			ErrInvariantViolation, parentOID, table.qualified())
	}
	if err := proveNodeIn(ctx, q, table); err != nil {
		return s, err
	}
	var versionNum int
	if err := q.QueryRow(ctx, `SELECT pg_catalog.current_setting('server_version_num')::pg_catalog.int4`).Scan(&versionNum); err != nil {
		return s, fmt.Errorf("read server_version_num: %w", err)
	}
	s.serverMajor = versionNum / 10000
	if s.parent, err = readParentIndexes(ctx, q, parentOID); err != nil {
		return s, err
	}
	s.table, err = readAttachTableIndexes(ctx, q, table)
	return s, err
}

const attachTableIndexesSQL = `SELECT ` + relationFactColumns + `
		   ` + relationFactJoins + `
		  WHERE i.indrelid OPERATOR(pg_catalog.=) $1
		    AND c.relkind OPERATOR(pg_catalog.=) 'i'
		  ORDER BY c.oid`

const attachTableConstraintsSQL = `SELECT k.conindid, k.contype::pg_catalog.text
		   FROM pg_catalog.pg_constraint k
		  WHERE k.conrelid OPERATOR(pg_catalog.=) $1
		    AND k.contype::pg_catalog.text OPERATOR(pg_catalog.=) ANY (ARRAY['p', 'u', 'x']::pg_catalog.text[])`

// readAttachTableIndexes reads every index of the table, in OID order, with
// the type of the index-owning constraint each backs.
func readAttachTableIndexes(ctx context.Context, q querier, table partitionNode) ([]attachTableIndex, error) {
	contypes := map[uint32]string{}
	rows, err := q.Query(ctx, attachTableConstraintsSQL, table.oid)
	if err != nil {
		return nil, fmt.Errorf("list the constraints of %s: %w", table.qualified(), err)
	}
	for rows.Next() {
		var (
			oid     uint32
			contype string
		)
		if err := rows.Scan(&oid, &contype); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan the constraints of %s: %w", table.qualified(), err)
		}
		contypes[oid] = contype
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the constraints of %s: %w", table.qualified(), err)
	}
	rows, err = q.Query(ctx, attachTableIndexesSQL, table.oid)
	if err != nil {
		return nil, fmt.Errorf("list the indexes of %s: %w", table.qualified(), err)
	}
	defer rows.Close()
	var found []attachTableIndex
	for rows.Next() {
		f, err := scanRelationFacts(rows)
		if err != nil {
			return nil, fmt.Errorf("scan the indexes of %s: %w", table.qualified(), err)
		}
		key, err := f.definitionKey()
		if err != nil {
			return nil, err
		}
		found = append(found, attachTableIndex{
			oid: f.oid, name: f.name, key: key, contype: contypes[f.oid], valid: f.valid, attached: f.isPartition,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the indexes of %s: %w", table.qualified(), err)
	}
	return found, nil
}
