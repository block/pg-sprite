package preflight

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// MaxColumnNumber is the highest column number PostgreSQL gives a table
// (MaxHeapAttributeNumber). A dropped column keeps its number until the
// table is rebuilt, so a table can reach it with far fewer live columns.
const MaxColumnNumber = 1600

// columnNumbersSQL reads the highest column number schema.table or any table
// that inherits from it has used. An ADD COLUMN reaches every such table, and
// each numbers its columns on its own, so the fullest one decides. A dropped
// column keeps its number, so the read counts dropped columns too.
const columnNumbersSQL = `WITH RECURSIVE tree(oid) AS (
	SELECT c.oid
	  FROM pg_catalog.pg_class c
	  JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
	 WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) $2
	UNION
	SELECT i.inhrelid
	  FROM pg_catalog.pg_inherits i
	  JOIN tree t ON i.inhparent OPERATOR(pg_catalog.=) t.oid
)
SELECT COALESCE(pg_catalog.max(a.attnum), 0)::pg_catalog.int4
  FROM pg_catalog.pg_attribute a
  JOIN tree t ON a.attrelid OPERATOR(pg_catalog.=) t.oid
 WHERE a.attnum OPERATOR(pg_catalog.>) 0`

// ColumnNumbersLeft reports how many more columns PostgreSQL lets an ADD
// COLUMN give schema.table: MaxColumnNumber less the highest column
// number the table, or a table that inherits from it, has ever used. It is
// zero once the fullest of them is at the limit.
func ColumnNumbersLeft(ctx context.Context, pool *pgxpool.Pool, schema, table string) (int, error) {
	var highest int32
	if err := pool.QueryRow(ctx, columnNumbersSQL, schema, table).Scan(&highest); err != nil {
		return 0, fmt.Errorf("read the column numbers %s.%s has used: %w", schema, table, err)
	}
	return max(MaxColumnNumber-int(highest), 0), nil
}
