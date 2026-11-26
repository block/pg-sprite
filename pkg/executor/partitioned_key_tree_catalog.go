package executor

import (
	"context"
	"fmt"
)

// The catalog reads the parent's step makes of every partition at once, under
// the tree's ACCESS EXCLUSIVE lock: the same facts readLeafKeyColumns and
// readLeafConstraints read of one leaf, in a fixed number of statements
// whatever the number of partitions, so the number of statements, and of
// round trips, under the lock does not grow with the tree. Every read runs
// under catalogReadPath, and every relation, function, operator, and cast is
// pg_catalog-qualified (CO-9).

// keyColumnsRead is one relation's key columns as one read found them: every
// key column in key order, or the first key column, in key order, the
// relation does not have.
type keyColumnsRead struct {
	columns []leafKeyColumn
	// lacksColumn is whether the relation does not have the key column
	// missing names; any name, the empty one included, can be missing.
	lacksColumn bool
	missing     string
}

// of returns the key columns, or the error readLeafKeyColumns returns for a
// relation without one of them. A relation the read did not cover has
// neither, and fails closed.
func (r keyColumnsRead) of(node partitionNode) ([]leafKeyColumn, error) {
	if r.lacksColumn {
		return nil, fmt.Errorf("table %s has no column %q for the key", node.qualified(), r.missing)
	}
	if len(r.columns) == 0 {
		return nil, fmt.Errorf("%w: the key columns of %s were not read", ErrInvariantViolation, node.qualified())
	}
	return r.columns, nil
}

// keyColumnsOfSQL is leafKeyColumnSQL for every relation in $1 and every key
// column in $2: one row per relation and key column, in key order, with
// NULL column facts when the relation has no live column of that name. The
// names travel as text and are compared with the column names as text, so a
// name too long to be a column name matches no column and is read as
// missing, as leafKeyColumnSQL reads it.
const keyColumnsOfSQL = `SELECT r.relid, w.name, a.attnum, a.attnotnull,
		        EXISTS (
		          SELECT 1 FROM pg_catalog.pg_constraint k
		           WHERE k.conrelid OPERATOR(pg_catalog.=) a.attrelid
		             AND k.contype::pg_catalog.text OPERATOR(pg_catalog.=) 'n'
		             AND k.conkey[1] OPERATOR(pg_catalog.=) a.attnum
		             AND NOT k.convalidated),
		        pg_catalog.quote_ident(a.attname)
		   FROM pg_catalog.unnest($1::pg_catalog.oid[]) AS r(relid)
		  CROSS JOIN pg_catalog.unnest($2::pg_catalog.text[]) WITH ORDINALITY AS w(name, ord)
		   LEFT JOIN pg_catalog.pg_attribute a
		          ON a.attrelid OPERATOR(pg_catalog.=) r.relid
		         AND a.attname::pg_catalog.text OPERATOR(pg_catalog.=) w.name
		         AND a.attnum OPERATOR(pg_catalog.>) 0
		         AND NOT a.attisdropped
		  ORDER BY r.relid, w.ord`

// readKeyColumnsOf reads the requested key columns of every node in one
// statement, keyed by the node's OID. A column refuses NULL as
// readLeafKeyColumns decides it: attnotnull set and, from PostgreSQL 18, no
// NOT VALID not-null constraint on it, under which it can still hold NULLs.
func readKeyColumnsOf(ctx context.Context, q querier, nodes []partitionNode, names []string) (map[uint32]keyColumnsRead, error) {
	oids := make([]uint32, 0, len(nodes))
	for _, n := range nodes {
		oids = append(oids, n.oid)
	}
	rows, err := q.Query(ctx, keyColumnsOfSQL, oids, names)
	if err != nil {
		return nil, fmt.Errorf("read the key columns of %d tables: %w", len(nodes), err)
	}
	defer rows.Close()
	found := make(map[uint32]keyColumnsRead, len(nodes))
	for rows.Next() {
		var (
			relid           uint32
			name            string
			attnum          *int16
			attNotNull      *bool
			notNullNotValid bool
			quoted          *string
		)
		if err := rows.Scan(&relid, &name, &attnum, &attNotNull, &notNullNotValid, &quoted); err != nil {
			return nil, fmt.Errorf("scan the key columns of %d tables: %w", len(nodes), err)
		}
		read := found[relid]
		switch {
		case read.lacksColumn:
			// A key column earlier in key order is already missing.
		case attnum == nil:
			read.lacksColumn, read.missing = true, name
		default:
			read.columns = append(read.columns, leafKeyColumn{
				name: name, attnum: *attnum, notNull: deref(attNotNull) && !notNullNotValid, quoted: deref(quoted),
			})
		}
		found[relid] = read
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the key columns of %d tables: %w", len(nodes), err)
	}
	return found, nil
}

// leafConstraintsRead is one leaf's PRIMARY KEY and UNIQUE constraints as one
// read found them, each with its index, or the first constraint whose index
// is not in the catalog.
type leafConstraintsRead struct {
	constraints []leafConstraint
	// orphan is the first constraint, in OID order, whose index orphanIndex
	// the catalog does not hold.
	orphan      *leafConstraint
	orphanIndex uint32
}

// of returns the constraints, or the error readLeafConstraints returns for a
// constraint whose index is not in the catalog.
func (r leafConstraintsRead) of(node partitionNode) ([]leafConstraint, error) {
	if r.orphan != nil {
		return nil, fmt.Errorf("%w: index %d of constraint %s on partition %s is not in the catalog",
			ErrInvariantViolation, r.orphanIndex, r.orphan.name, node.qualified())
	}
	return r.constraints, nil
}

// leafConstraintsOfSQL is leafConstraintsSQL for every leaf in $1.
const leafConstraintsOfSQL = `SELECT k.conrelid, k.oid, k.conname, k.contype::pg_catalog.text, k.conindid, k.conparentid, k.condeferrable, k.conkey
		   FROM pg_catalog.pg_constraint k
		  WHERE k.conrelid OPERATOR(pg_catalog.=) ANY ($1::pg_catalog.oid[])
		    AND k.contype::pg_catalog.text OPERATOR(pg_catalog.=) ANY (ARRAY['p', 'u']::pg_catalog.text[])
		  ORDER BY k.conrelid, k.oid`

// relationsByOIDSQL is relationByOIDSQL for every relation in $1.
const relationsByOIDSQL = `SELECT ` + relationFactColumns + `
		   ` + relationFactJoins + `
		  WHERE c.oid OPERATOR(pg_catalog.=) ANY ($1::pg_catalog.oid[])`

// readLeafConstraintsOf lists the PRIMARY KEY and UNIQUE constraints of every
// leaf in one statement and reads the index behind each in one more, keyed by
// the leaf's OID. A leaf without such a constraint has no entry.
func readLeafConstraintsOf(ctx context.Context, q querier, leaves []partitionNode) (map[uint32]leafConstraintsRead, error) {
	oids := make([]uint32, 0, len(leaves))
	for _, n := range leaves {
		oids = append(oids, n.oid)
	}
	type listed struct {
		leaf     uint32
		c        leafConstraint
		indexOID uint32
	}
	rows, err := q.Query(ctx, leafConstraintsOfSQL, oids)
	if err != nil {
		return nil, fmt.Errorf("list the key constraints of %d partitions: %w", len(leaves), err)
	}
	var (
		all      []listed
		indexOID []uint32
	)
	for rows.Next() {
		var l listed
		if err := rows.Scan(&l.leaf, &l.c.oid, &l.c.name, &l.c.contype, &l.indexOID, &l.c.parentOID, &l.c.deferrable, &l.c.key); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan the key constraints of %d partitions: %w", len(leaves), err)
		}
		all = append(all, l)
		indexOID = append(indexOID, l.indexOID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the key constraints of %d partitions: %w", len(leaves), err)
	}
	indexes, err := relationsByOID(ctx, q, indexOID)
	if err != nil {
		return nil, err
	}
	found := make(map[uint32]leafConstraintsRead, len(leaves))
	for _, l := range all {
		read := found[l.leaf]
		idx, ok := indexes[l.indexOID]
		switch {
		case read.orphan != nil:
			// A constraint earlier in OID order already has no index.
		case !ok:
			orphan := l.c
			read.orphan, read.orphanIndex = &orphan, l.indexOID
		default:
			l.c.index = idx
			read.constraints = append(read.constraints, l.c)
		}
		found[l.leaf] = read
	}
	return found, nil
}

// relationsByOID reads every relation in oids in one statement, keyed by OID.
// A relation the catalog does not hold has no entry.
func relationsByOID(ctx context.Context, q querier, oids []uint32) (map[uint32]relationFacts, error) {
	rows, err := q.Query(ctx, relationsByOIDSQL, oids)
	if err != nil {
		return nil, fmt.Errorf("read %d relations: %w", len(oids), err)
	}
	defer rows.Close()
	found := make(map[uint32]relationFacts, len(oids))
	for rows.Next() {
		f, err := scanRelationFacts(rows)
		if err != nil {
			return nil, fmt.Errorf("scan %d relations: %w", len(oids), err)
		}
		found[f.oid] = f
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read %d relations: %w", len(oids), err)
	}
	return found, nil
}
