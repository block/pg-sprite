package preflight

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrTypeNotFound means the type a privilege check was asked about no
// longer exists: another session dropped it after the engine resolved it.
var ErrTypeNotFound = errors.New("the type is no longer in the catalog")

// CheckTypeOwnership verifies the connected role may alter the type with
// OID typ. ALTER TYPE needs membership in the type's owning role, the same
// rung a table's in-place ALTER TABLE needs, so a missing membership is the
// *PrivilegeError that rung reports, with a hint that transfers the type
// instead.
func CheckTypeOwnership(ctx context.Context, pool *pgxpool.Pool, typ uint32) error {
	// INV: CO-9 — every function, operator, relation, and type is
	// pg_catalog-qualified, so a planted overload on the session's
	// search_path cannot answer the ownership probe true.
	const q = `
		SELECT current_user::pg_catalog.text,
		       pg_catalog.current_setting('server_version_num')::int,
		       n.nspname::pg_catalog.text,
		       t.typname::pg_catalog.text,
		       r.rolname::pg_catalog.text,
		       pg_catalog.pg_has_role(current_user, t.typowner, 'USAGE'),
		       (SELECT rolinherit FROM pg_catalog.pg_roles WHERE rolname OPERATOR(pg_catalog.=) current_user)
		FROM pg_catalog.pg_type t
		JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) t.typnamespace
		JOIN pg_catalog.pg_roles r ON r.oid OPERATOR(pg_catalog.=) t.typowner
		WHERE t.oid OPERATOR(pg_catalog.=) $1`
	// The refusal is built from the facts a table's ladder gathers; the
	// type's name stands where the table's would.
	var f accessFacts
	err := pool.QueryRow(ctx, q, typ).Scan(&f.role, &f.versionNum, &f.schema, &f.table, &f.owner, &f.ownerUsage, &f.roleInherit)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: OID %d", ErrTypeNotFound, typ)
	}
	if err != nil {
		return fmt.Errorf("read the owner of type OID %d: %w", typ, err)
	}
	if !f.ownerUsage {
		return ownerMembershipRefusal(f, "type")
	}
	return nil
}
