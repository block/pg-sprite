package preflight

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrSchemaNotFound is returned when the schema a create would target does
// not exist. Creating a table cannot fix a missing schema, so the cause is
// separated from a free name.
var ErrSchemaNotFound = errors.New("schema not found")

// ErrRelationExists is returned when the target name is already occupied by
// a relation of any kind — a table, view, index, or sequence all block a
// CREATE TABLE at that name the same way.
var ErrRelationExists = errors.New("a relation already exists at the target name")

// AbsentTarget proves the target name is free: the schema exists and no
// relation of any kind occupies schema.table. It can only be constructed by
// CheckTableAbsent in this package. The schema it carries is always
// resolved — an unqualified check records the session's creation schema, so
// the proof names the exact schema a create would land in.
type AbsentTarget struct {
	schema string
	table  string
}

// Schema returns the resolved schema the absence was verified in. It is
// never empty: an unqualified check resolves the session's creation schema
// before verifying.
func (a AbsentTarget) Schema() string { return a.schema }

// Table returns the verified-absent table name.
func (a AbsentTarget) Table() string { return a.table }

// CheckTableAbsent verifies that schema.table names no existing relation,
// so a CREATE TABLE at that name has nothing to collide with. When schema
// is empty the session's creation schema (current_schema()) is resolved
// first — the schema an unqualified CREATE TABLE would land in — and the
// proof carries it. The facts come from one catalog snapshot read directly
// from pg_class, which is visible regardless of privileges, so a missing
// grant can never masquerade as absence; whether the role may create in
// the schema is a separate privilege check, not this fact check.
func CheckTableAbsent(ctx context.Context, pool *pgxpool.Pool, schema, table string) (AbsentTarget, error) {
	// One row always comes back: the LEFT JOINs turn "schema missing" and
	// "name free" into NULL columns instead of absent rows, so the causes
	// are separated from one snapshot that cannot disagree with itself.
	const q = `
		SELECT s.nspname,
		       n.nspname IS NOT NULL,
		       c.relkind::text
		FROM (SELECT CASE WHEN $1 = '' THEN current_schema() ELSE $1 END AS nspname) s
		LEFT JOIN pg_namespace n ON n.nspname = s.nspname
		LEFT JOIN pg_class c ON c.relnamespace = n.oid AND c.relname = $2`
	var targetSchema, relkind *string
	var schemaExists bool
	if err := pool.QueryRow(ctx, q, schema, table).Scan(&targetSchema, &schemaExists, &relkind); err != nil {
		return AbsentTarget{}, fmt.Errorf("check absence of %s: %w", qualifiedName(schema, table), err)
	}
	if targetSchema == nil {
		// Only an unqualified check can land here: current_schema() is
		// NULL when the search_path names no usable schema, so there is
		// no schema to verify the absence in.
		return AbsentTarget{}, fmt.Errorf("resolve creation schema for %s: the session's search_path names no schema", table)
	}
	if !schemaExists {
		return AbsentTarget{}, fmt.Errorf("%w: schema %s does not exist", ErrSchemaNotFound, *targetSchema)
	}
	if relkind != nil {
		return AbsentTarget{}, fmt.Errorf("%w: %s has relkind %q",
			ErrRelationExists, qualifiedName(*targetSchema, table), *relkind)
	}
	return AbsentTarget{schema: *targetSchema, table: table}, nil
}
