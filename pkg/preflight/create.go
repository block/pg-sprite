// This file is the create path's privilege check. A greenfield CREATE
// TABLE has no owner to be a member of — the table is born owned by the
// role that creates it — so the check proves the off-ladder TierCreateTable
// facts (CONNECT on the database, USAGE and CREATE on the schema) instead
// of walking the ownership tier ladder in privileges.go, which states
// facts about an existing table.

package preflight

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrCreateOwnerNotFound is returned when the named create owner is not a
// role on the server. The name is caller configuration, so the cause is
// separated from a missing grant: no GRANT provisions a role that does not
// exist, and nothing has run.
var ErrCreateOwnerNotFound = errors.New("create owner role not found")

// CreationRole proves the connected role holds every access a greenfield
// CREATE TABLE in the schema needs: CONNECT on the database, USAGE and
// CREATE on the schema for the role the table will be owned by, and — when
// a create owner other than the connected role is named — membership that
// lets the session SET ROLE to it. It can only be constructed by
// CheckCreatePrivileges or CheckCreatePrivilegesAs in this package. The
// schema it carries is always resolved — an unqualified check records the
// session's creation schema.
//
// Like AbsentTarget, the proof is time-of-check and session-scoped: a
// grant can be revoked between the check and the CREATE TABLE, in which
// case the create fails with the server's own insufficient-privilege
// error rather than a typed refusal.
type CreationRole struct {
	role   string
	owner  string
	schema string
}

// Role returns the connected role the checks ran as — the identity whose
// CONNECT was proved and the grantee of any membership the create needs.
func (c CreationRole) Role() string { return c.role }

// Owner returns the role a created table will be owned by: the named create
// owner when one was checked, otherwise the connected role.
func (c CreationRole) Owner() string { return c.owner }

// SetsRole reports whether create steps must SET LOCAL ROLE to Owner
// because it differs from the connected role.
func (c CreationRole) SetsRole() bool { return c.owner != c.role }

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
	return CheckCreatePrivilegesAs(ctx, pool, schema, "")
}

// CheckCreatePrivilegesAs verifies creation access for owner. When owner is
// empty, creation remains under the connected role. Otherwise the connected
// role must be able to SET ROLE to owner, and owner must hold schema access.
//
// SET ROLE consults membership, not inheritance: pg_has_role(..., 'MEMBER')
// is the predicate on every supported version, and PostgreSQL 16 adds the
// SET membership option as a second, narrower gate. A non-inheriting member
// can assume the owner and is admitted; the USAGE mode — whether the owner's
// privileges are already available without SET ROLE — is not what the
// mechanism needs, and a refusal keyed on it could not be cleared by the
// GRANT it prints.
func CheckCreatePrivilegesAs(ctx context.Context, pool *pgxpool.Pool, schema, owner string) (CreationRole, error) {
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
		       current_setting('server_version_num')::int,
		       CASE WHEN $2 = '' THEN true ELSE EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $2) END,
		       CASE WHEN $2 = '' OR $2 = current_user THEN true
		            ELSE COALESCE(pg_has_role(current_user, (SELECT oid FROM pg_roles WHERE rolname = $2), 'MEMBER'), false) END,
		       has_database_privilege(current_user, current_database(), 'CONNECT'),
		       COALESCE(has_schema_privilege(CASE WHEN $2 = '' THEN current_user::regrole::oid ELSE (SELECT oid FROM pg_roles WHERE rolname = $2) END, n.oid, 'USAGE'), false),
		       COALESCE(has_schema_privilege(CASE WHEN $2 = '' THEN current_user::regrole::oid ELSE (SELECT oid FROM pg_roles WHERE rolname = $2) END, n.oid, 'CREATE'), false)
		FROM (SELECT CASE WHEN $1 = '' THEN current_schema() ELSE $1 END AS nspname) s
		LEFT JOIN pg_namespace n ON n.nspname = s.nspname`
	var targetSchema *string
	var schemaExists, ownerExists, ownerMember, canConnect, schemaUsage, schemaCreate bool
	var versionNum int
	var role, database string
	if err := pool.QueryRow(ctx, q, schema, owner).Scan(
		&targetSchema, &schemaExists, &role, &database,
		&versionNum, &ownerExists, &ownerMember, &canConnect, &schemaUsage, &schemaCreate); err != nil {
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
	if !ownerExists {
		return CreationRole{}, fmt.Errorf("%w: create owner role %q does not exist", ErrCreateOwnerNotFound, owner)
	}
	// INV: ST-6 — each missing grant is a typed refusal carrying the exact
	// provisioning statement; the proof is only minted when every fact
	// holds.
	if !canConnect {
		return CreationRole{}, connectRefusal(role, database)
	}
	createRole := role
	if owner != "" {
		createRole = owner
	}
	setsRole := createRole != role
	if !ownerMember {
		grant := fmt.Sprintf("GRANT %s TO %s", pgx.Identifier{owner}.Sanitize(), pgx.Identifier{role}.Sanitize())
		if versionNum >= 160000 {
			grant += " WITH SET TRUE"
		}
		return CreationRole{}, &PrivilegeError{Tier: TierCreateTable,
			Check: fmt.Sprintf("pg_has_role(%s, %s, 'MEMBER')", role, owner), Grant: grant}
	}
	if !schemaUsage {
		return CreationRole{}, schemaUsageRefusal(createRole, *targetSchema)
	}
	if !schemaCreate {
		return CreationRole{}, &PrivilegeError{
			Tier:  TierCreateTable,
			Check: fmt.Sprintf("has_schema_privilege(%s, %s, 'CREATE')", createRole, *targetSchema),
			Grant: fmt.Sprintf("GRANT CREATE ON SCHEMA %s TO %s",
				pgx.Identifier{*targetSchema}.Sanitize(), pgx.Identifier{createRole}.Sanitize()),
		}
	}
	if setsRole {
		if err := checkSetRoleAccess(ctx, pool, accessFacts{role: role, owner: owner, versionNum: versionNum}); err != nil {
			var privilegeErr *PrivilegeError
			if errors.As(err, &privilegeErr) {
				privilegeErr.Tier = TierCreateTable
			}
			return CreationRole{}, err
		}
	}
	return CreationRole{role: role, owner: createRole, schema: *targetSchema}, nil
}
