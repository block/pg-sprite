package schemadiff

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
)

// Querier is the part of a pgx connection, transaction, or pooled
// connection that a catalog read needs.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ReadDroppedObject reads, from the live catalog, the object a destructive
// change removes, rendered exactly as Diff renders it into a DroppedObject:
// the table schema.table is resolved by qualified name, and the index,
// constraint, column, or NOT NULL called name is read by the introspection
// queries a plan is derived from. found is false when the table or the
// object does not exist.
//
// Definitions the server decompiles print under the session's search_path,
// so the caller must read under the one introspection uses: the schema,
// then public. The read takes no lock; the caller holds whatever lock must
// keep the answer true until the drop.
func ReadDroppedObject(ctx context.Context, q Querier, schema, table string, kind DroppedKind, name string) (live DroppedObject, found bool, err error) {
	var oid uint32
	// INV: CO-9 — every catalog name in this query is pg_catalog-qualified.
	err = q.QueryRow(ctx, `
		SELECT c.oid
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) $2
		  AND c.relkind OPERATOR(pg_catalog.=) ANY (ARRAY['r', 'p']::pg_catalog."char"[])`, schema, table).Scan(&oid)
	if errors.Is(err, pgx.ErrNoRows) {
		return DroppedObject{}, false, nil
	}
	if err != nil {
		return DroppedObject{}, false, fmt.Errorf("resolve table %s.%s: %w", schema, table, err)
	}
	switch kind {
	case DroppedIndex:
		indexes, err := introspectIndexes(ctx, q, oid, nil)
		if err != nil {
			return DroppedObject{}, false, fmt.Errorf("read the indexes of %s.%s: %w", schema, table, err)
		}
		for _, ix := range indexes {
			if ix.Name == name {
				return DroppedObject{Kind: kind, Name: name, Definition: ix.Def}, true, nil
			}
		}
	case DroppedConstraint:
		constraints, err := introspectConstraints(ctx, q, oid, false)
		if err != nil {
			return DroppedObject{}, false, fmt.Errorf("read the constraints of %s.%s: %w", schema, table, err)
		}
		for _, con := range constraints {
			if con.Name == name {
				return DroppedObject{Kind: kind, Name: name, Definition: con.Def}, true, nil
			}
		}
	case DroppedColumn, DroppedNotNull:
		columns, err := introspectColumns(ctx, q, oid)
		if err != nil {
			return DroppedObject{}, false, fmt.Errorf("read the columns of %s.%s: %w", schema, table, err)
		}
		for _, col := range columns {
			if col.Name != name {
				continue
			}
			if kind == DroppedColumn {
				return DroppedObject{
					Kind: kind, Name: name, Definition: col.DroppedDefinition(),
					Dependents: DependentLines(col.Dependents), DependentObjects: slices.Clone(col.Dependents),
				}, true, nil
			}
			if !col.NotNull {
				return DroppedObject{Kind: kind, Name: name, Definition: fmt.Sprintf("attnum %d: nullable", col.AttNum)}, true, nil
			}
			return DroppedObject{Kind: kind, Name: name, Definition: col.DroppedNotNullDefinition()}, true, nil
		}
	default:
		return DroppedObject{}, false, fmt.Errorf("unknown dropped object kind %q", kind)
	}
	return DroppedObject{}, false, nil
}
