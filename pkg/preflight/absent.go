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

// ErrTypeExists is returned when the target name is already occupied by a
// standalone type — an enum, domain, range, or shell type. Every table gets
// a composite type of the same name, so a CREATE TABLE collides with these
// exactly as it does with a relation.
var ErrTypeExists = errors.New("a type already exists at the target name")

// ErrNoCreationSchema is returned when an unqualified check cannot resolve
// the session's creation schema: the search_path names no usable schema, so
// there is no schema to verify the absence in. Unlike the other refusals
// this one is not a database fact — the remedy is entirely on the caller's
// side: qualify the name, or fix the connection's search_path.
var ErrNoCreationSchema = errors.New("the session's search_path names no creation schema")

// IsNameOccupied reports whether err means the target name is already held,
// by a relation or by a standalone type. Both block a CREATE TABLE the same
// way, so a caller routing "name taken" versus "name free" matches this
// predicate rather than the two sentinels separately — the sentinels stay
// distinct for messages, where the difference tells an operator what the
// obstacle actually is.
func IsNameOccupied(err error) bool {
	return errors.Is(err, ErrRelationExists) || errors.Is(err, ErrTypeExists)
}

// AbsentTarget proves the target name is free: the schema exists and no
// relation or standalone type occupies schema.table. It can only be
// constructed by CheckTableAbsent in this package. The schema it carries is
// always resolved — an unqualified check records the session's creation
// schema, so the proof names the exact schema a create would land in.
//
// The proof is time-of-check and session-scoped: mint it inside the apply,
// in the same session that will run the CREATE TABLE, and never serialize
// it or carry it across a plan/apply boundary — an absence verified at plan
// time proves nothing about apply time. The create path must also re-verify
// it at the point of use, the way ST-7 does for PreflightedTable: reject a
// proof whose Table() is empty (the zero value is forgeable by any package),
// and require the CREATE TABLE statement's schema and table to equal the
// proof's before executing.
type AbsentTarget struct {
	schema string
	table  string
}

// Schema returns the resolved schema the absence was verified in. A proof
// minted by CheckTableAbsent always carries one: an unqualified check
// resolves the session's creation schema before verifying.
func (a AbsentTarget) Schema() string { return a.schema }

// Table returns the verified-absent table name.
func (a AbsentTarget) Table() string { return a.table }

// CheckTableAbsent verifies that schema.table names no existing relation or
// standalone type, so a CREATE TABLE at that name has nothing to collide
// with. When schema is empty the session's creation schema
// (current_schema()) is resolved first — the schema an unqualified CREATE
// TABLE would land in — and the proof carries it. The facts come from one
// catalog snapshot read directly from pg_class and pg_type, which are
// visible regardless of privileges, so a missing grant can never masquerade
// as absence; whether the role may create in the schema is a separate
// privilege check, not this fact check. The proof is time-of-check: nothing
// locks the name, so a concurrent create can still take it before the
// CREATE TABLE runs — the create path must still treat a duplicate-name
// error as a collision; the proof turns the common case into a clean
// refusal, not a guarantee.
//
// This check is NOT the complement of CheckTable for an unqualified name:
// CheckTable resolves across the whole search_path (to_regclass), while
// this check resolves current_schema() only — the one schema an unqualified
// CREATE TABLE lands in. A table in a later search_path schema makes both
// checks succeed for the same arguments: CheckTable finds it, and this
// check correctly reports the creation schema free. A caller deciding
// between create and alter on that pairing would create a new table that
// shadows the one the user meant — so such a caller must pass an explicit
// schema, where the two checks share one namespace and are true inverses.
func CheckTableAbsent(ctx context.Context, pool *pgxpool.Pool, schema, table string) (AbsentTarget, error) {
	if table == "" {
		return AbsentTarget{}, fmt.Errorf("check absence in schema %q: empty table name", schema)
	}
	// One row always comes back: the LEFT JOINs turn "schema missing" and
	// "name free" into NULL columns instead of absent rows, so the causes
	// are separated from one snapshot that cannot disagree with itself —
	// pg_class and pg_type are each unique on (name, namespace), so each
	// join matches at most once. The two joins are NOT always disjoint:
	// an index has no pg_type row, so a standalone type can share its
	// name and both columns come back non-NULL. The relkind-before-typtype
	// branch order below resolves that double occupant deliberately — the
	// relation is what blocks a CREATE TABLE, and reporting ErrTypeExists
	// would send an operator chasing a type that is not the obstacle. The
	// typrelid = 0 filter keeps ErrTypeExists meaning *standalone* type
	// (every relation owns the pg_type row of its name), so the branch
	// order stays a tie-break rather than load-bearing correctness. The
	// typelem/typarray filter drops autogenerated array types (typelem set,
	// no array of their own): CREATE TABLE renames those out of the way
	// rather than colliding.
	const q = `
		SELECT s.nspname,
		       n.nspname IS NOT NULL,
		       c.relkind::text,
		       ty.typtype::text
		FROM (SELECT CASE WHEN $1 = '' THEN current_schema() ELSE $1 END AS nspname) s
		LEFT JOIN pg_namespace n ON n.nspname = s.nspname
		LEFT JOIN pg_class c ON c.relnamespace = n.oid AND c.relname = $2
		LEFT JOIN pg_type ty ON ty.typnamespace = n.oid AND ty.typname = $2
			AND ty.typrelid = 0 AND (ty.typelem = 0 OR ty.typarray <> 0)`
	var targetSchema, relkind, typtype *string
	var schemaExists bool
	if err := pool.QueryRow(ctx, q, schema, table).Scan(&targetSchema, &schemaExists, &relkind, &typtype); err != nil {
		return AbsentTarget{}, fmt.Errorf("check absence of %s: %w", qualifiedName(schema, table), err)
	}
	if targetSchema == nil {
		// Only an unqualified check can land here: current_schema() is
		// NULL when the search_path names no usable schema, so there is
		// no schema to verify the absence in.
		return AbsentTarget{}, fmt.Errorf("resolve creation schema for %s: %w", table, ErrNoCreationSchema)
	}
	if !schemaExists {
		return AbsentTarget{}, fmt.Errorf("%w: schema %s does not exist", ErrSchemaNotFound, *targetSchema)
	}
	if relkind != nil {
		return AbsentTarget{}, fmt.Errorf("%w: %s has relkind %q",
			ErrRelationExists, qualifiedName(*targetSchema, table), *relkind)
	}
	if typtype != nil {
		return AbsentTarget{}, fmt.Errorf("%w: %s has typtype %q",
			ErrTypeExists, qualifiedName(*targetSchema, table), *typtype)
	}
	return AbsentTarget{schema: *targetSchema, table: table}, nil
}
