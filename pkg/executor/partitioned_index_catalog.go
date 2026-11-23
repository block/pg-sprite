package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// The catalog reads of the partitioned index build. Every one runs on a session
// whose search_path is pg_catalog alone (catalogReadPath), so a definition the
// server prints names every schema its objects live in, and every relation and
// operator is pg_catalog-qualified (CO-9).

// relationFacts is what one catalog read says about a relation found by name
// or by identity: its kind and, for an index, the facts every decision on it
// turns on. The index fields are zero for any other relation.
type relationFacts struct {
	oid     uint32
	schema  string
	name    string
	relkind string
	// isPartition is pg_class.relispartition: for an index, it is attached to
	// a partitioned index.
	isPartition  bool
	tableOID     uint32
	tableName    string
	tableRelkind string
	valid        bool
	unique       bool
	def          string
	quotedName   string
	quotedSchema string
	quotedTable  string
	// backsConstraint is whether a constraint names the relation as its
	// index (pg_constraint.conindid).
	backsConstraint bool
}

func (f relationFacts) isPartitionedIndex() bool { return f.relkind == "I" }

// relationFactColumns reads relationFacts from pg_class row c, its namespace n,
// and, for an index, its pg_index row i and table t (all left-joined, so a table
// or sequence comes back with NULL index facts). It must run on a session whose
// search_path is pg_catalog alone, which makes the definition fully qualified.
const relationFactColumns = `c.oid, n.nspname, c.relname, c.relkind::pg_catalog.text, c.relispartition,
		        i.indrelid, i.indisvalid, i.indisunique,
		        CASE WHEN c.relkind OPERATOR(pg_catalog.=) 'i' OR c.relkind OPERATOR(pg_catalog.=) 'I'
		             THEN pg_catalog.pg_get_indexdef(c.oid) END,
		        pg_catalog.quote_ident(c.relname), pg_catalog.quote_ident(n.nspname),
		        pg_catalog.quote_ident(t.relname), t.relkind::pg_catalog.text, t.relname,
		        EXISTS (SELECT 1 FROM pg_catalog.pg_constraint k WHERE k.conindid OPERATOR(pg_catalog.=) c.oid)`

const relationFactJoins = `FROM pg_catalog.pg_class c
		   JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		   LEFT JOIN pg_catalog.pg_index i ON i.indexrelid OPERATOR(pg_catalog.=) c.oid
		   LEFT JOIN pg_catalog.pg_class t ON t.oid OPERATOR(pg_catalog.=) i.indrelid`

func scanRelationFacts(row pgx.Row) (relationFacts, error) {
	var (
		f                              relationFacts
		tableOID                       *uint32
		valid, unique                  *bool
		def, quotedTable, tableRelkind *string
		tableName                      *string
	)
	err := row.Scan(&f.oid, &f.schema, &f.name, &f.relkind, &f.isPartition,
		&tableOID, &valid, &unique, &def,
		&f.quotedName, &f.quotedSchema, &quotedTable, &tableRelkind, &tableName, &f.backsConstraint)
	if err != nil {
		return relationFacts{}, err
	}
	f.tableOID, f.valid, f.unique = deref(tableOID), deref(valid), deref(unique)
	f.def, f.quotedTable = deref(def), deref(quotedTable)
	f.tableRelkind, f.tableName = deref(tableRelkind), deref(tableName)
	return f, nil
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

const relationByNameSQL = `SELECT ` + relationFactColumns + `
		   ` + relationFactJoins + `
		  WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) $2`

const relationByOIDSQL = `SELECT ` + relationFactColumns + `
		   ` + relationFactJoins + `
		  WHERE c.oid OPERATOR(pg_catalog.=) $1`

// relationByName reads the relation that owns a name in a schema; a name is
// unique among a schema's relations.
func relationByName(ctx context.Context, q querier, schema, name string) (relationFacts, bool, error) {
	f, err := scanRelationFacts(q.QueryRow(ctx, relationByNameSQL, schema, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return relationFacts{}, false, nil
	}
	if err != nil {
		return relationFacts{}, false, fmt.Errorf("read relation %s: %w", qualifiedName(schema, name), err)
	}
	return f, true, nil
}

// relationByOID reads a relation by identity.
func relationByOID(ctx context.Context, q querier, oid uint32) (relationFacts, bool, error) {
	f, err := scanRelationFacts(q.QueryRow(ctx, relationByOIDSQL, oid))
	if errors.Is(err, pgx.ErrNoRows) {
		return relationFacts{}, false, nil
	}
	if err != nil {
		return relationFacts{}, false, fmt.Errorf("read relation %d: %w", oid, err)
	}
	return f, true, nil
}

// reusableIndexesSQL selects, with the same columns as relationByNameSQL, the
// indexes on the table $1 that could stand in for the one the build would
// create: valid, ready, attached to no partitioned index, and backing no
// constraint. A constraint's index has duties beyond its key, and attaching it
// under a parent index that is not a constraint's would change what it is.
const reusableIndexesSQL = `SELECT ` + relationFactColumns + `
		   ` + relationFactJoins + `
		  WHERE i.indrelid OPERATOR(pg_catalog.=) $1
		    AND c.relkind OPERATOR(pg_catalog.=) 'i'
		    AND NOT c.relispartition
		    AND i.indisvalid AND i.indisready
		    AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint k
		                     WHERE k.conindid OPERATOR(pg_catalog.=) c.oid)
		  ORDER BY c.oid`

// reusableIndexes lists the indexes on a leaf partition that could stand in for
// the index the build would create. The caller compares each one's definition.
func reusableIndexes(ctx context.Context, q querier, tableOID uint32) ([]relationFacts, error) {
	rows, err := q.Query(ctx, reusableIndexesSQL, tableOID)
	if err != nil {
		return nil, fmt.Errorf("list indexes of table %d: %w", tableOID, err)
	}
	defer rows.Close()
	var found []relationFacts
	for rows.Next() {
		f, err := scanRelationFacts(rows)
		if err != nil {
			return nil, fmt.Errorf("scan indexes of table %d: %w", tableOID, err)
		}
		found = append(found, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list indexes of table %d: %w", tableOID, err)
	}
	return found, nil
}

const attachedIndexOfSQL = `SELECT i.indexrelid
		   FROM pg_catalog.pg_index i
		   JOIN pg_catalog.pg_inherits h ON h.inhrelid OPERATOR(pg_catalog.=) i.indexrelid
		  WHERE i.indrelid OPERATOR(pg_catalog.=) $1
		    AND h.inhparent OPERATOR(pg_catalog.=) $2`

// attachedIndexOf returns the index on the partition that is attached to the
// partitioned index parentIndexOID, if the partition has one.
func attachedIndexOf(ctx context.Context, q querier, partitionOID, parentIndexOID uint32) (uint32, bool, error) {
	var oid uint32
	err := q.QueryRow(ctx, attachedIndexOfSQL, partitionOID, parentIndexOID).Scan(&oid)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read the index of partition %d attached to index %d: %w", partitionOID, parentIndexOID, err)
	}
	return oid, true, nil
}

// definitionKey is the part of an index's printed definition that two indexes
// must share for the server to accept one as a partition of the other:
// uniqueness and everything from the access method on. The name and the table
// are cut off by their exact printed form, not by searching for a keyword,
// which a quoted name could contain. A definition that does not begin with the
// form the server is known to print fails closed, so a server that changes the
// format stops the build instead of matching wrongly.
func (f relationFacts) definitionKey() (string, error) {
	prefix := "CREATE "
	if f.unique {
		prefix += "UNIQUE "
	}
	prefix += "INDEX " + f.quotedName + " ON "
	if f.tableRelkind == "p" {
		prefix += "ONLY "
	}
	prefix += f.quotedSchema + "." + f.quotedTable
	tail, ok := strings.CutPrefix(f.def, prefix)
	if !ok {
		return "", fmt.Errorf("%w: the definition of index %s.%s is not in the form the build reads: %q",
			ErrInvariantViolation, f.schema, f.name, f.def)
	}
	return fmt.Sprintf("unique=%t%s", f.unique, tail), nil
}

// describeRelation names what a relation is, for a refusal that says it is not
// the index the build needs.
func describeRelation(r relationFacts) string {
	switch r.relkind {
	case "r":
		return "a table"
	case "p":
		return "a partitioned table"
	case "i":
		return "an index on " + qualifiedName(r.schema, r.tableName)
	case "I":
		return "a partitioned index on " + qualifiedName(r.schema, r.tableName)
	case "S":
		return "a sequence"
	case "v", "m":
		return "a view"
	default:
		return "a relation of kind " + r.relkind
	}
}
