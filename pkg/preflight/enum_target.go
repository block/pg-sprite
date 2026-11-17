package preflight

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/statement"
)

// ErrEnumTypeNotFound means the type an ALTER TYPE ... ADD VALUE names is
// not an enum type visible on the session search_path. Nothing has
// executed.
var ErrEnumTypeNotFound = errors.New("the type ADD VALUE names is not an enum type visible on the session search_path")

// EnumTarget is the proof that the enum type a statement names resolved,
// on the session search_path, to an enum the connected role may alter. It
// carries the name it was resolved from and the type's OID, schema, and
// name. It is time-of-check: another session can rename the type after
// minting, so the executor confirms by identity, before its commit, that
// its statement reached the type with this OID (ST-7).
type EnumTarget struct {
	ref    statement.TypeName
	oid    uint32
	schema string
	name   string
}

// Ref returns the type name, as the statement spelled it, that the proof
// was resolved from.
func (e EnumTarget) Ref() statement.TypeName { return e.ref }

// OID returns the resolved type's OID; zero for a proof no check minted.
func (e EnumTarget) OID() uint32 { return e.oid }

// Schema returns the schema the type lives in.
func (e EnumTarget) Schema() string { return e.schema }

// Qualified returns the type's schema-qualified name for operator-facing
// text.
func (e EnumTarget) Qualified() string { return e.schema + "." + e.name }

// CheckEnumTarget resolves the type ref names and mints the proof that the
// connected role may alter it. An unqualified name resolves on the session
// search_path, exactly as the statement will when it runs on the same
// pool. A name that is not an enum type is ErrEnumTypeNotFound. ALTER TYPE
// needs membership in the type's owning role, the same rung a table's
// in-place ALTER TABLE needs, so a missing membership is the
// *PrivilegeError that rung reports, with a hint that transfers the type
// instead; a qualified name in a schema the role lacks USAGE on is the
// *PrivilegeError that carries the schema grant.
func CheckEnumTarget(ctx context.Context, pool *pgxpool.Pool, ref statement.TypeName) (EnumTarget, error) {
	e, f, err := resolveEnumType(ctx, pool, ref)
	if err != nil {
		return EnumTarget{}, err
	}
	if !f.ownerUsage {
		return EnumTarget{}, ownerMembershipRefusal(f, "type")
	}
	return e, nil
}

// LookupEnumSchema returns the schema of the enum type ref names, resolved
// as CheckEnumTarget resolves it, without checking that the role may alter
// the type. It is the fact a plan reports, never a proof.
func LookupEnumSchema(ctx context.Context, pool *pgxpool.Pool, ref statement.TypeName) (string, error) {
	e, _, err := resolveEnumType(ctx, pool, ref)
	if err != nil {
		return "", err
	}
	return e.schema, nil
}

// resolveEnumType reads, in one query, the enum type ref names and the
// facts the ownership rung judges. Resolving a qualified name in a schema
// the role cannot use raises a permission error rather than finding no
// type, so that schema's USAGE is checked first.
func resolveEnumType(ctx context.Context, pool *pgxpool.Pool, ref statement.TypeName) (EnumTarget, accessFacts, error) {
	name := pgx.Identifier{ref.Name}
	if ref.Schema != "" {
		name = pgx.Identifier{ref.Schema, ref.Name}
		if err := checkTypeSchemaUsage(ctx, pool, ref.Schema); err != nil {
			return EnumTarget{}, accessFacts{}, err
		}
	}
	// INV: CO-9 — every function, operator, relation, and type is
	// pg_catalog-qualified, so a planted overload on the session's
	// search_path cannot change which type the facts describe or answer
	// the ownership probe true. to_regtype itself still resolves the bare
	// name on that path, exactly as the statement will.
	const q = `
		SELECT t.oid,
		       t.typtype OPERATOR(pg_catalog.=) 'e',
		       current_user::pg_catalog.text,
		       pg_catalog.current_setting('server_version_num')::int,
		       n.nspname::pg_catalog.text,
		       t.typname::pg_catalog.text,
		       r.rolname::pg_catalog.text,
		       pg_catalog.pg_has_role(current_user, t.typowner, 'USAGE'),
		       (SELECT rolinherit FROM pg_catalog.pg_roles WHERE rolname OPERATOR(pg_catalog.=) current_user)
		FROM pg_catalog.pg_type t
		JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) t.typnamespace
		JOIN pg_catalog.pg_roles r ON r.oid OPERATOR(pg_catalog.=) t.typowner
		WHERE t.oid OPERATOR(pg_catalog.=) pg_catalog.to_regtype($1)`
	// The refusal is built from the facts a table's ladder gathers; the
	// type's name stands where the table's would.
	e := EnumTarget{ref: ref}
	var f accessFacts
	var isEnum bool
	err := pool.QueryRow(ctx, q, name.Sanitize()).Scan(&e.oid, &isEnum,
		&f.role, &f.versionNum, &f.schema, &f.table, &f.owner, &f.ownerUsage, &f.roleInherit)
	if errors.Is(err, pgx.ErrNoRows) {
		return EnumTarget{}, accessFacts{}, fmt.Errorf("%w: no type %s; nothing was executed", ErrEnumTypeNotFound, name.Sanitize())
	}
	if err != nil {
		return EnumTarget{}, accessFacts{}, fmt.Errorf("resolve enum type %s: %w", name.Sanitize(), err)
	}
	e.schema, e.name = f.schema, f.table
	if !isEnum {
		return EnumTarget{}, accessFacts{}, fmt.Errorf("%w: %s is not an enum type; nothing was executed", ErrEnumTypeNotFound, e.Qualified())
	}
	return e, f, nil
}

// checkTypeSchemaUsage verifies the connected role has USAGE on schema,
// the schema a qualified type name spells, and returns the
// *PrivilegeError that carries the grant when it does not. A schema that
// does not exist passes: resolving the name then finds no type.
func checkTypeSchemaUsage(ctx context.Context, pool *pgxpool.Pool, schema string) error {
	// INV: CO-9 — pg_catalog-qualified like every privilege probe.
	const q = `
		SELECT current_user::pg_catalog.text,
		       pg_catalog.has_schema_privilege(current_user, n.oid, 'USAGE')
		FROM pg_catalog.pg_namespace n
		WHERE n.nspname OPERATOR(pg_catalog.=) $1`
	var role string
	var usage bool
	err := pool.QueryRow(ctx, q, schema).Scan(&role, &usage)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read USAGE on schema %s: %w", schema, err)
	}
	if !usage {
		return schemaUsageRefusal(role, schema)
	}
	return nil
}
