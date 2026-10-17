package testutil

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/statement"
)

// TableCatalog is a table's definition as the server's catalogs record it,
// read with direct catalog queries and never through pkg/schemadiff. It is
// the reference side of the round-trip oracle: two tables whose catalogs
// match are the same table, whatever the diff model records about them. A
// property the model does not carry still differs here, so a plan that
// converges the model but not the table fails the comparison instead of
// re-planning to silence.
//
// Definitions are decompiled under a search_path of the table's own schema,
// so a table in one schema compares equal to the same table in another.
// Policies, triggers, grants, comments, and ownership are not part of it.
type TableCatalog struct {
	// Persistence is pg_class.relpersistence: 'p' permanent, 'u' unlogged.
	Persistence string
	// AccessMethod is the table access method (pg_class.relam), "heap"
	// for an ordinary table and empty when it has none.
	AccessMethod string
	// Tablespace is the table's tablespace, empty for the database
	// default.
	Tablespace string
	// ReplicaIdentity is pg_class.relreplident: 'd' default, 'n' nothing,
	// 'f' full, 'i' index.
	ReplicaIdentity string
	// Options are the storage parameters (pg_class.reloptions), e.g.
	// "fillfactor=70", in the order the server stores them.
	Options []string
	// ToastOptions are the storage parameters of the table's TOAST table,
	// set with the "toast." prefix, in the order the server stores them.
	ToastOptions []string
	// PartitionKey is pg_get_partkeydef, empty for a plain table.
	PartitionKey string
	// RowSecurity and ForceRowSecurity are the ENABLE / FORCE ROW LEVEL
	// SECURITY settings.
	RowSecurity      bool
	ForceRowSecurity bool
	// Columns are the live columns in attribute order.
	Columns []CatalogColumn
	// Constraints are the table constraints, name-sorted.
	Constraints []CatalogConstraint
	// Indexes are every index on the table, constraint-backed ones
	// included, name-sorted.
	Indexes []CatalogIndex
}

// CatalogColumn is one column's catalog record.
type CatalogColumn struct {
	Name    string
	Type    string
	NotNull bool
	// Default is the default or generation expression, empty when none.
	Default string
	// Identity is pg_attribute.attidentity: empty, 'a' always, 'd' by
	// default.
	Identity string
	// Generated is pg_attribute.attgenerated: empty, 's' stored, 'v'
	// virtual.
	Generated string
	// Collation is the column's collation when it differs from its type's
	// default, empty otherwise.
	Collation string
	// Storage is pg_attribute.attstorage.
	Storage string
	// Compression is pg_attribute.attcompression, empty for the default.
	Compression string
	// StatisticsTarget is the per-column statistics target, -1 for the
	// default.
	StatisticsTarget int
	// Options are the column's attribute options (pg_attribute.attoptions),
	// e.g. "n_distinct=100", in the order the server stores them.
	Options []string
	// Sequence is the sequence the column owns — a serial's or an identity
	// column's — and nil when it owns none.
	Sequence *CatalogSequence
}

// CatalogSequence is the parameters of a column-owned sequence. Its name is
// left out: a serial's sequence is named after the table, and the
// parameters are what the column's values depend on.
type CatalogSequence struct {
	Type      string
	Start     int64
	Increment int64
	Min       int64
	Max       int64
	Cache     int64
	Cycle     bool
}

// CatalogConstraint is one table constraint's catalog record.
type CatalogConstraint struct {
	Name string
	// Type is pg_constraint.contype.
	Type string
	// Def is pg_get_constraintdef, which spells NOT VALID, NO INHERIT, and
	// deferrability.
	Def       string
	Validated bool
}

// CatalogIndex is one index's catalog record.
type CatalogIndex struct {
	Name string
	// Def is pg_get_indexdef with the ON clause's schema stripped: the
	// server always qualifies it, whatever the search_path.
	Def             string
	Valid           bool
	ReplicaIdentity bool
	Clustered       bool
}

// ReadTableCatalog reads the catalog record of schema.table in one
// read-only snapshot.
func ReadTableCatalog(ctx context.Context, pool *pgxpool.Pool, schema, table string) (TableCatalog, error) {
	var tc TableCatalog
	err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL search_path = "+pgx.Identifier{schema}.Sanitize()); err != nil {
			return fmt.Errorf("set search_path to %s: %w", schema, err)
		}
		rel := pgx.Identifier{table}.Sanitize()
		if err := tx.QueryRow(ctx, `SELECT c.relpersistence::text, coalesce(am.amname::text, ''),
       coalesce(ts.spcname::text, ''), c.relreplident::text,
       coalesce(c.reloptions, '{}'),
       coalesce(toast.reloptions, '{}'),
       coalesce(pg_get_partkeydef(c.oid), ''),
       c.relrowsecurity, c.relforcerowsecurity
FROM pg_class c
LEFT JOIN pg_am am ON am.oid = c.relam
LEFT JOIN pg_tablespace ts ON ts.oid = c.reltablespace
LEFT JOIN pg_class toast ON toast.oid = c.reltoastrelid
WHERE c.oid = $1::regclass`, rel).Scan(&tc.Persistence, &tc.AccessMethod, &tc.Tablespace, &tc.ReplicaIdentity,
			&tc.Options, &tc.ToastOptions, &tc.PartitionKey, &tc.RowSecurity, &tc.ForceRowSecurity); err != nil {
			return fmt.Errorf("read table %s.%s: %w", schema, table, err)
		}
		columns, err := readCatalogColumns(ctx, tx, rel)
		if err != nil {
			return fmt.Errorf("read columns of %s.%s: %w", schema, table, err)
		}
		tc.Columns = columns
		constraints, err := readCatalogConstraints(ctx, tx, rel)
		if err != nil {
			return fmt.Errorf("read constraints of %s.%s: %w", schema, table, err)
		}
		tc.Constraints = constraints
		indexes, err := readCatalogIndexes(ctx, tx, rel)
		if err != nil {
			return fmt.Errorf("read indexes of %s.%s: %w", schema, table, err)
		}
		tc.Indexes = indexes
		return nil
	})
	return tc, err
}

func readCatalogColumns(ctx context.Context, tx pgx.Tx, rel string) ([]CatalogColumn, error) {
	// The owned sequence is joined through its pg_depend edge: 'a' for a
	// serial's OWNED BY, 'i' for an identity column's internal sequence.
	// The edge is joined together with pg_sequence, so the other pg_class
	// objects that depend on a column, such as each index over it, never
	// add a row.
	rows, err := tx.Query(ctx, `SELECT a.attname, format_type(a.atttypid, a.atttypmod), a.attnotnull,
       coalesce(pg_get_expr(d.adbin, d.adrelid), ''),
       a.attidentity::text, a.attgenerated::text,
       CASE WHEN a.attcollation <> t.typcollation THEN a.attcollation::regcollation::text ELSE '' END,
       a.attstorage::text, a.attcompression::text,
       coalesce(a.attstattarget::int, -1),
       coalesce(a.attoptions, '{}'),
       s.seqtypid IS NOT NULL,
       coalesce(format_type(s.seqtypid, NULL), ''),
       coalesce(s.seqstart, 0), coalesce(s.seqincrement, 0),
       coalesce(s.seqmin, 0), coalesce(s.seqmax, 0),
       coalesce(s.seqcache, 0), coalesce(s.seqcycle, false)
FROM pg_attribute a
JOIN pg_type t ON t.oid = a.atttypid
LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
LEFT JOIN (pg_depend dep JOIN pg_sequence s ON s.seqrelid = dep.objid)
       ON dep.classid = 'pg_class'::regclass
      AND dep.refclassid = 'pg_class'::regclass
      AND dep.refobjid = a.attrelid
      AND dep.refobjsubid = a.attnum
      AND dep.deptype IN ('a', 'i')
WHERE a.attrelid = $1::regclass AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY a.attnum`, rel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []CatalogColumn
	for rows.Next() {
		var c CatalogColumn
		var hasSequence bool
		var seq CatalogSequence
		if err := rows.Scan(&c.Name, &c.Type, &c.NotNull, &c.Default, &c.Identity, &c.Generated,
			&c.Collation, &c.Storage, &c.Compression, &c.StatisticsTarget, &c.Options,
			&hasSequence, &seq.Type, &seq.Start, &seq.Increment, &seq.Min, &seq.Max, &seq.Cache, &seq.Cycle); err != nil {
			return nil, err
		}
		if hasSequence {
			c.Sequence = &seq
		}
		columns = append(columns, c)
	}
	return columns, rows.Err()
}

func readCatalogConstraints(ctx context.Context, tx pgx.Tx, rel string) ([]CatalogConstraint, error) {
	rows, err := tx.Query(ctx, `SELECT conname, contype::text, pg_get_constraintdef(oid), convalidated
FROM pg_constraint
WHERE conrelid = $1::regclass
ORDER BY conname`, rel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var constraints []CatalogConstraint
	for rows.Next() {
		var c CatalogConstraint
		if err := rows.Scan(&c.Name, &c.Type, &c.Def, &c.Validated); err != nil {
			return nil, err
		}
		constraints = append(constraints, c)
	}
	return constraints, rows.Err()
}

func readCatalogIndexes(ctx context.Context, tx pgx.Tx, rel string) ([]CatalogIndex, error) {
	rows, err := tx.Query(ctx, `SELECT c.relname, pg_get_indexdef(i.indexrelid), i.indisvalid, i.indisreplident, i.indisclustered
FROM pg_index i
JOIN pg_class c ON c.oid = i.indexrelid
WHERE i.indrelid = $1::regclass
ORDER BY c.relname`, rel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var indexes []CatalogIndex
	for rows.Next() {
		var ix CatalogIndex
		if err := rows.Scan(&ix.Name, &ix.Def, &ix.Valid, &ix.ReplicaIdentity, &ix.Clustered); err != nil {
			return nil, err
		}
		if ix.Def, err = statement.Qualify(ix.Def, ""); err != nil {
			return nil, fmt.Errorf("unqualify index %s: %w", ix.Name, err)
		}
		indexes = append(indexes, ix)
	}
	return indexes, rows.Err()
}
