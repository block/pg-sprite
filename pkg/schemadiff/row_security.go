package schemadiff

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// ErrUnrenderableRowSecurity means exporting a table would omit its RLS
// settings or policies. Desired files cannot represent these objects yet.
var ErrUnrenderableRowSecurity = errors.New("row security cannot be rendered as a desired schema")

// PolicyCommand identifies the operations a policy applies to.
type PolicyCommand string

// PostgreSQL's pg_policy.polcmd values, including ALL (not a wildcard role).
const (
	PolicyAll    PolicyCommand = "*"
	PolicySelect PolicyCommand = "r"
	PolicyInsert PolicyCommand = "a"
	PolicyUpdate PolicyCommand = "w"
	PolicyDelete PolicyCommand = "d"
)

// RowSecurity is observed table-local access control, not a desired-state
// ownership declaration. Diff refuses a desired model carrying it. Enabled and Forced
// are independent: PostgreSQL retains FORCE and policies while RLS is disabled.
type RowSecurity struct {
	Enabled  bool
	Forced   bool
	Policies []Policy
}

// Policy is a pg_policy catalog entry. Expressions come from pg_get_expr with
// only pg_catalog on search_path, retaining schema qualification for external
// functions and relations. This is an inventory, not a proof that two expressions
// grant equivalent access or that their dependencies have unchanged definitions.
type Policy struct {
	Name       string
	Command    PolicyCommand
	Permissive bool
	// Roles contains name-sorted roles; "public" represents PostgreSQL's
	// special PUBLIC audience (OID zero), not a provisioned application role.
	Roles []string
	// Using and WithCheck preserve NULL separately from an explicit expression.
	// In particular, an omitted WITH CHECK can inherit USING semantics.
	Using     *string
	WithCheck *string
	Comment   *string
}

func (r RowSecurity) present() bool {
	return r.Enabled || r.Forced || len(r.Policies) != 0
}

// introspectRowSecurity runs last: policy expressions must retain external
// qualification even when the referenced object lives in the table's schema.
func introspectRowSecurity(ctx context.Context, tx pgx.Tx, oid uint32) (RowSecurity, error) {
	if _, err := tx.Exec(ctx, dbconn.LocalSearchPath("pg_catalog")); err != nil {
		return RowSecurity{}, fmt.Errorf("set policy introspection search_path: %w", err)
	}
	var security RowSecurity
	if err := tx.QueryRow(ctx, `
		SELECT relrowsecurity, relforcerowsecurity
		FROM pg_catalog.pg_class WHERE oid = $1`, oid).
		Scan(&security.Enabled, &security.Forced); err != nil {
		return RowSecurity{}, fmt.Errorf("read row security settings: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT p.polname, p.polcmd::text, p.polpermissive,
		       ARRAY(
		           SELECT CASE WHEN audience.role_oid = 0 THEN 'public' ELSE r.rolname::text END
		           FROM pg_catalog.unnest(p.polroles) AS audience(role_oid)
		           LEFT JOIN pg_catalog.pg_roles r ON r.oid = audience.role_oid
		           ORDER BY 1
		       ),
		       pg_catalog.pg_get_expr(p.polqual, p.polrelid),
		       pg_catalog.pg_get_expr(p.polwithcheck, p.polrelid),
		       pg_catalog.obj_description(p.oid, 'pg_policy')
		FROM pg_catalog.pg_policy p
		WHERE p.polrelid = $1
		ORDER BY p.polname`, oid)
	if err != nil {
		return RowSecurity{}, fmt.Errorf("query row security policies: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var policy Policy
		if err := rows.Scan(&policy.Name, &policy.Command, &policy.Permissive,
			&policy.Roles, &policy.Using, &policy.WithCheck, &policy.Comment); err != nil {
			return RowSecurity{}, fmt.Errorf("scan row security policy: %w", err)
		}
		security.Policies = append(security.Policies, policy)
	}
	if err := rows.Err(); err != nil {
		return RowSecurity{}, fmt.Errorf("read row security policies: %w", err)
	}
	return security, nil
}
