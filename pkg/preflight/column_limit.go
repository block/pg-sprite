package preflight

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MaxColumnNumber is the highest column number PostgreSQL gives a table
// (MaxHeapAttributeNumber). A dropped column keeps its number until the
// table is rebuilt, so a table can reach it with far fewer live columns.
const MaxColumnNumber = 1600

// ColumnNumbers is how many more columns PostgreSQL lets an ADD COLUMN give a
// table, and which tables decide it.
type ColumnNumbers struct {
	// Left is MaxColumnNumber less the highest column number the table, or a
	// table that inherits from it, has ever used: zero once the fullest of
	// them is at the limit.
	Left int `json:"numbers_left"`
	// Fullest are the tables that have used that highest number, the table
	// itself first when it is one of them, then the rest by name. Rebuilding
	// them is what gives numbers back.
	Fullest []FullTable `json:"full_tables"`
}

// FullTable is a table that has used the highest column number among a table
// and the tables that inherit from it.
type FullTable struct {
	// Name is the table, schema-qualified with every identifier quoted.
	Name string `json:"name"`
	// Root is true for the table the ADD COLUMN names, and false for a
	// partition or inheritance child it reaches.
	Root bool `json:"altered_table"`
	// Partition is true for a partition, which is rebuilt by detaching it
	// and attaching a copy, rather than for an inheritance child.
	Partition bool `json:"partition"`
}

// columnNumbersSQL reads the highest column number schema.table and each
// table that inherits from it has used. An ADD COLUMN reaches every such
// table, and each numbers its columns on its own, so the fullest one decides.
// A dropped column keeps its number, so the read counts dropped columns too.
const columnNumbersSQL = `WITH RECURSIVE root(oid) AS (
	SELECT c.oid
	  FROM pg_catalog.pg_class c
	  JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
	 WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) $2
), tree(oid) AS (
	SELECT oid FROM root
	UNION
	SELECT i.inhrelid
	  FROM pg_catalog.pg_inherits i
	  JOIN tree t ON i.inhparent OPERATOR(pg_catalog.=) t.oid
)
SELECT n.nspname, c.relname,
       c.oid OPERATOR(pg_catalog.=) (SELECT oid FROM root),
       c.relispartition,
       COALESCE((SELECT pg_catalog.max(a.attnum)
                   FROM pg_catalog.pg_attribute a
                  WHERE a.attrelid OPERATOR(pg_catalog.=) c.oid
                    AND a.attnum OPERATOR(pg_catalog.>) 0), 0)::pg_catalog.int4
  FROM tree t
  JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) t.oid
  JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace`

// ReadColumnNumbers reads how many more columns PostgreSQL lets an ADD COLUMN
// give schema.table, and which of it and the tables that inherit from it have
// used the most numbers.
func ReadColumnNumbers(ctx context.Context, pool *pgxpool.Pool, schema, table string) (ColumnNumbers, error) {
	rows, err := pool.Query(ctx, columnNumbersSQL, schema, table)
	if err != nil {
		return ColumnNumbers{}, fmt.Errorf("read the column numbers %s.%s has used: %w", schema, table, err)
	}
	defer rows.Close()
	type used struct {
		table   FullTable
		highest int32
	}
	var tables []used
	for rows.Next() {
		var relSchema, relName string
		var u used
		if err := rows.Scan(&relSchema, &relName, &u.table.Root, &u.table.Partition, &u.highest); err != nil {
			return ColumnNumbers{}, fmt.Errorf("read the column numbers %s.%s has used: %w", schema, table, err)
		}
		u.table.Name = pgx.Identifier{relSchema, relName}.Sanitize()
		tables = append(tables, u)
	}
	if err := rows.Err(); err != nil {
		return ColumnNumbers{}, fmt.Errorf("read the column numbers %s.%s has used: %w", schema, table, err)
	}
	var highest int32
	for _, u := range tables {
		highest = max(highest, u.highest)
	}
	numbers := ColumnNumbers{Left: max(MaxColumnNumber-int(highest), 0)}
	for _, u := range tables {
		if u.highest == highest && highest > 0 {
			numbers.Fullest = append(numbers.Fullest, u.table)
		}
	}
	slices.SortFunc(numbers.Fullest, func(a, b FullTable) int {
		if a.Root != b.Root {
			if a.Root {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	return numbers, nil
}
