// This file is the create path's privilege check. A greenfield CREATE
// TABLE has no owner to be a member of — the table is born owned by the
// role that creates it — so the check proves the off-ladder TierCreateTable
// facts (CONNECT on the database, USAGE and CREATE on the schema) instead
// of walking the ownership tier ladder in privileges.go, which states
// facts about an existing table.

package preflight

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CreationRole proves the connected role holds every access a greenfield
// CREATE TABLE in the schema needs: CONNECT on the database, USAGE and
// CREATE on the schema. It can only be constructed by
// CheckCreatePrivileges in this package. The schema it carries is always
// resolved — an unqualified check records the session's creation schema.
//
// Like AbsentTarget, the proof is time-of-check and session-scoped: a
// grant can be revoked between the check and the CREATE TABLE, in which
// case the create fails with the server's own insufficient-privilege
// error rather than a typed refusal.
type CreationRole struct {
	role   string
	schema string
}

// Role returns the connected role the checks ran as — the role a created
// table would be owned by.
func (c CreationRole) Role() string { return c.role }

// Schema returns the resolved schema the access was verified in.
func (c CreationRole) Schema() string { return c.schema }

// CheckCreatePrivileges verifies the connected role can create a table in
// the schema (the session's creation schema, current_schema(), when schema
// is empty): CONNECT on the database, USAGE and CREATE on the schema. A
// missing grant is a *PrivilegeError naming the exact statement that would
// satisfy it — the grantee is the connected role itself, because a table
// that does not exist yet has no owning role to inherit from. On success
// it returns the CreationRole proof.
func CheckCreatePrivileges(ctx context.Context, pool *pgxpool.Pool, schema string) (CreationRole, error) {
	// One catalog snapshot gathers every fact the check consults, so the
	// facts cannot disagree about when they looked. The LEFT JOIN turns
	// "schema missing" into a false exists column instead of an absent
	// row, and COALESCE keeps the privilege probes NULL-safe on that
	// branch.
	const q = `
		SELECT s.nspname,
		       n.nspname IS NOT NULL,
		       current_user::text,
		       current_database()::text,
		       has_database_privilege(current_user, current_database(), 'CONNECT'),
		       COALESCE(has_schema_privilege(current_user, n.nspname, 'USAGE'), false),
		       COALESCE(has_schema_privilege(current_user, n.nspname, 'CREATE'), false)
		FROM (SELECT CASE WHEN $1 = '' THEN current_schema() ELSE $1 END AS nspname) s
		LEFT JOIN pg_namespace n ON n.nspname = s.nspname`
	var targetSchema *string
	var schemaExists, canConnect, schemaUsage, schemaCreate bool
	var role, database string
	if err := pool.QueryRow(ctx, q, schema).Scan(
		&targetSchema, &schemaExists, &role, &database,
		&canConnect, &schemaUsage, &schemaCreate); err != nil {
		return CreationRole{}, fmt.Errorf("gather create access facts for schema %q: %w", schema, err)
	}
	if targetSchema == nil {
		// Only an unqualified check can land here: current_schema() is
		// NULL when the search_path names no usable schema, so there is
		// no schema to check creation access in.
		return CreationRole{}, fmt.Errorf("resolve creation schema: %w", ErrNoCreationSchema)
	}
	if !schemaExists {
		return CreationRole{}, fmt.Errorf("%w: schema %s does not exist", ErrSchemaNotFound, *targetSchema)
	}
	// INV: ST-6 — each missing grant is a typed refusal carrying the exact
	// provisioning statement; the proof is only minted when every fact
	// holds.
	if !canConnect {
		return CreationRole{}, &PrivilegeError{
			Tier:  TierConnect,
			Check: fmt.Sprintf("has_database_privilege(%s, %s, 'CONNECT')", role, database),
			Grant: fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s",
				pgx.Identifier{database}.Sanitize(), pgx.Identifier{role}.Sanitize()),
		}
	}
	if !schemaUsage {
		return CreationRole{}, &PrivilegeError{
			Tier:  TierConnect,
			Check: fmt.Sprintf("has_schema_privilege(%s, %s, 'USAGE')", role, *targetSchema),
			Grant: fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s",
				pgx.Identifier{*targetSchema}.Sanitize(), pgx.Identifier{role}.Sanitize()),
		}
	}
	if !schemaCreate {
		return CreationRole{}, &PrivilegeError{
			Tier:  TierCreateTable,
			Check: fmt.Sprintf("has_schema_privilege(%s, %s, 'CREATE')", role, *targetSchema),
			Grant: fmt.Sprintf("GRANT CREATE ON SCHEMA %s TO %s",
				pgx.Identifier{*targetSchema}.Sanitize(), pgx.Identifier{role}.Sanitize()),
		}
	}
	return CreationRole{role: role, schema: *targetSchema}, nil
}
