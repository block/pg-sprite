package schemachange

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// FidelitySnapshot is the table metadata that CREATE TABLE … LIKE INCLUDING
// ALL does not carry and the shadow builder therefore replicates itself:
// the ST-5 checklist minus the data-side facts (sequence positions, index
// validity) that only exist once rows have been copied. Cutover re-reads
// the same snapshot from both tables and refuses to swap unless they match.
type FidelitySnapshot struct {
	// Owner is the catalog owner of the table.
	Owner string
	// ReplicaIdentity is pg_class.relreplident: 'd' for DEFAULT, 'f' for FULL.
	ReplicaIdentity string
	// RLSEnabled and RLSForced are the row-level-security switches.
	RLSEnabled bool
	// RLSForced reports FORCE ROW LEVEL SECURITY.
	RLSForced bool
	// Comment is the table comment, empty when none.
	Comment string
	// Tablespace is the table's own tablespace, empty when it lives in the
	// database default. LIKE places the shadow in the default tablespace
	// whatever the source uses; the builder moves the still-empty shadow.
	Tablespace string
	// RelOptions are the table's storage parameters as the server prints
	// them, with the TOAST relation's parameters prefixed "toast.".
	RelOptions []string
	// Grants are the table's effective privileges, expanded from its ACL
	// (or from the owner's default ACL when none is stored): one entry per
	// privilege and grantee, grantable when any grantor made it so.
	Grants []Grant
	// ColumnGrants are the effective privileges granted on individual
	// columns.
	ColumnGrants []ColumnGrant
	// Policies are the table's row-level-security policies.
	Policies []Policy
	// UnvalidatedChecks are the CHECK constraints the user left NOT VALID.
	// LIKE copies them as validated; the builder re-adds them NOT VALID so
	// the copier accepts every row the source legally holds.
	UnvalidatedChecks []UnvalidatedConstraint
}

// Grant is one effective ACL entry.
type Grant struct {
	// Privilege is the privilege keyword as aclexplode reports it.
	Privilege string
	// Grantee is the role name; empty when Public is set.
	Grantee string
	// Public reports the PUBLIC pseudo-role (grantee OID 0). It is carried
	// as a fact of its own because a real role may be named "PUBLIC": the
	// server reserves only the folded spelling "public".
	Public bool
	// Grantable reports WITH GRANT OPTION.
	Grantable bool
}

// ColumnGrant is one effective per-column ACL entry; its Privilege is
// SELECT, INSERT, UPDATE, or REFERENCES.
type ColumnGrant struct {
	// Column is the column carrying the ACL.
	Column string
	Grant
}

// Policy is one row-level-security policy.
type Policy struct {
	// Name is the policy name.
	Name string
	// Permissive reports AS PERMISSIVE (false is AS RESTRICTIVE).
	Permissive bool
	// Command is the pg_policy.polcmd code: '*', 'r', 'a', 'w', or 'd'.
	Command string
	// Roles are the real role names the policy applies to.
	Roles []string
	// AppliesToPublic reports TO PUBLIC, which the server stores as the
	// single pseudo-role OID 0 in place of any role list.
	AppliesToPublic bool
	// Using is the decompiled USING expression, empty when none.
	Using string
	// WithCheck is the decompiled WITH CHECK expression, empty when none.
	WithCheck string
}

// UnvalidatedConstraint is one CHECK constraint whose pg_constraint row has
// convalidated = false, with its server-decompiled definition (which ends in
// NOT VALID).
type UnvalidatedConstraint struct {
	// Name is the constraint name.
	Name string
	// Def is the pg_get_constraintdef text.
	Def string
}

// publicKeyword is how GRANT and CREATE POLICY spell the PUBLIC pseudo-role.
// It is a keyword, never quoted: quoting it would name a real role.
const publicKeyword = "PUBLIC"

// isTablePrivilege reports whether the server-reported privilege keyword is
// one this code knows a table can carry; anything else means a server this
// code does not know, and the builder refuses rather than splice unknown
// text into a GRANT.
func isTablePrivilege(privilege string) bool {
	switch privilege {
	case "SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER", "MAINTAIN":
		return true
	default:
		return false
	}
}

// isColumnPrivilege reports whether the server-reported privilege keyword is
// one this code knows a column can carry.
func isColumnPrivilege(privilege string) bool {
	switch privilege {
	case "SELECT", "INSERT", "UPDATE", "REFERENCES":
		return true
	default:
		return false
	}
}

// policyCommand maps a pg_policy.polcmd code to its CREATE POLICY keyword.
func policyCommand(code string) (string, bool) {
	switch code {
	case "*":
		return "ALL", true
	case "r":
		return "SELECT", true
	case "a":
		return "INSERT", true
	case "w":
		return "UPDATE", true
	case "d":
		return "DELETE", true
	default:
		return "", false
	}
}

// readFidelity reads the snapshot for the relation with the given OID.
func readFidelity(ctx context.Context, tx pgx.Tx, oid uint32) (FidelitySnapshot, error) {
	var s FidelitySnapshot
	var toastOptions []string
	err := tx.QueryRow(ctx, `
		SELECT pg_get_userbyid(c.relowner), c.relreplident::text,
		       c.relrowsecurity, c.relforcerowsecurity,
		       COALESCE(obj_description(c.oid, 'pg_class'), ''),
		       COALESCE((SELECT t.spcname FROM pg_tablespace t WHERE t.oid = c.reltablespace), ''),
		       COALESCE(c.reloptions, '{}'),
		       COALESCE((SELECT t.reloptions FROM pg_class t WHERE t.oid = c.reltoastrelid), '{}')
		FROM pg_class c
		WHERE c.oid = $1`, oid).
		Scan(&s.Owner, &s.ReplicaIdentity, &s.RLSEnabled, &s.RLSForced, &s.Comment, &s.Tablespace, &s.RelOptions, &toastOptions)
	if err != nil {
		return FidelitySnapshot{}, fmt.Errorf("read table metadata: %w", err)
	}
	for _, opt := range toastOptions {
		s.RelOptions = append(s.RelOptions, "toast."+opt)
	}
	if s.Grants, err = readGrants(ctx, tx, oid); err != nil {
		return FidelitySnapshot{}, err
	}
	if s.ColumnGrants, err = readColumnGrants(ctx, tx, oid); err != nil {
		return FidelitySnapshot{}, err
	}
	if s.Policies, err = readPolicies(ctx, tx, oid); err != nil {
		return FidelitySnapshot{}, err
	}
	if s.UnvalidatedChecks, err = readUnvalidatedChecks(ctx, tx, oid); err != nil {
		return FidelitySnapshot{}, err
	}
	return s, nil
}

// readGrants reads the table's effective ACL: aclexplode yields one row per
// grantor, so entries are folded per privilege and grantee, grantable when
// any grantor made them so. The PUBLIC pseudo-role is recognised by its OID,
// never by a name the catalog can also print for a real role.
func readGrants(ctx context.Context, tx pgx.Tx, oid uint32) ([]Grant, error) {
	rows, err := tx.Query(ctx, `
		SELECT a.privilege_type,
		       CASE WHEN a.grantee = 0 THEN '' ELSE pg_get_userbyid(a.grantee) END,
		       a.grantee = 0,
		       bool_or(a.is_grantable)
		FROM pg_class c
		CROSS JOIN LATERAL aclexplode(COALESCE(c.relacl, acldefault('r', c.relowner))) a
		WHERE c.oid = $1
		GROUP BY 1, 2, 3
		ORDER BY 3, 2, 1`, oid)
	if err != nil {
		return nil, fmt.Errorf("read table grants: %w", err)
	}
	grants, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Grant, error) {
		var g Grant
		err := row.Scan(&g.Privilege, &g.Grantee, &g.Public, &g.Grantable)
		return g, err
	})
	if err != nil {
		return nil, fmt.Errorf("read table grants: %w", err)
	}
	for _, g := range grants {
		if !isTablePrivilege(g.Privilege) {
			return nil, fmt.Errorf("table grant to %s carries unknown privilege %q", granteeLabel(g), g.Privilege)
		}
	}
	return grants, nil
}

// readColumnGrants reads the effective per-column ACLs, folded per column,
// privilege, and grantee like readGrants.
func readColumnGrants(ctx context.Context, tx pgx.Tx, oid uint32) ([]ColumnGrant, error) {
	rows, err := tx.Query(ctx, `
		SELECT col.attname, acl.privilege_type,
		       CASE WHEN acl.grantee = 0 THEN '' ELSE pg_get_userbyid(acl.grantee) END,
		       acl.grantee = 0,
		       bool_or(acl.is_grantable)
		FROM pg_attribute col
		CROSS JOIN LATERAL aclexplode(col.attacl) acl
		WHERE col.attrelid = $1 AND col.attnum > 0 AND NOT col.attisdropped
		GROUP BY col.attnum, 1, 2, 3, 4
		ORDER BY col.attnum, 4, 3, 2`, oid)
	if err != nil {
		return nil, fmt.Errorf("read column grants: %w", err)
	}
	grants, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ColumnGrant, error) {
		var g ColumnGrant
		err := row.Scan(&g.Column, &g.Privilege, &g.Grantee, &g.Public, &g.Grantable)
		return g, err
	})
	if err != nil {
		return nil, fmt.Errorf("read column grants: %w", err)
	}
	for _, g := range grants {
		if !isColumnPrivilege(g.Privilege) {
			return nil, fmt.Errorf("column grant on %s to %s carries unknown privilege %q", g.Column, granteeLabel(g.Grant), g.Privilege)
		}
	}
	return grants, nil
}

// readPolicies reads the table's policies. A policy TO PUBLIC is stored as
// the single pseudo-role OID 0, which is reported as AppliesToPublic and
// kept out of Roles.
func readPolicies(ctx context.Context, tx pgx.Tx, oid uint32) ([]Policy, error) {
	rows, err := tx.Query(ctx, `
		SELECT p.polname, p.polpermissive, p.polcmd::text,
		       ARRAY(SELECT pg_get_userbyid(r)::text FROM unnest(p.polroles) r WHERE r <> 0),
		       0 = ANY (p.polroles),
		       COALESCE(pg_get_expr(p.polqual, p.polrelid), ''),
		       COALESCE(pg_get_expr(p.polwithcheck, p.polrelid), '')
		FROM pg_policy p
		WHERE p.polrelid = $1
		ORDER BY p.polname`, oid)
	if err != nil {
		return nil, fmt.Errorf("read row-level-security policies: %w", err)
	}
	policies, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Policy, error) {
		var p Policy
		err := row.Scan(&p.Name, &p.Permissive, &p.Command, &p.Roles, &p.AppliesToPublic, &p.Using, &p.WithCheck)
		return p, err
	})
	if err != nil {
		return nil, fmt.Errorf("read row-level-security policies: %w", err)
	}
	for _, p := range policies {
		if _, known := policyCommand(p.Command); !known {
			return nil, fmt.Errorf("policy %s carries unknown command code %q", p.Name, p.Command)
		}
	}
	return policies, nil
}

func readUnvalidatedChecks(ctx context.Context, tx pgx.Tx, oid uint32) ([]UnvalidatedConstraint, error) {
	rows, err := tx.Query(ctx, `
		SELECT conname, pg_get_constraintdef(oid)
		FROM pg_constraint
		WHERE conrelid = $1 AND contype = 'c' AND NOT convalidated
		ORDER BY conname`, oid)
	if err != nil {
		return nil, fmt.Errorf("read unvalidated check constraints: %w", err)
	}
	checks, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (UnvalidatedConstraint, error) {
		var c UnvalidatedConstraint
		err := row.Scan(&c.Name, &c.Def)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("read unvalidated check constraints: %w", err)
	}
	return checks, nil
}

// applyFidelity replicates the snapshot onto the freshly created, still
// empty shadow. Ownership is not applied here: the shadow is created under
// SET LOCAL ROLE owner, so it is owner-correct from birth and the builder verifies
// that instead. Every role and relation name goes through Sanitize; the
// comment goes through the server's format(%L) so no literal is spliced by
// hand; expressions, options, and constraint definitions are the server's
// own decompiled text. The ACLs are synchronised rather than granted: the
// owner's default privileges land on the shadow at CREATE and are not part
// of the source's ACL, so the shadow's effective grants are made equal to
// the source's and re-read to prove it.
func applyFidelity(ctx context.Context, tx pgx.Tx, schema, shadow string, shadowOID uint32, s FidelitySnapshot) error {
	table := pgx.Identifier{schema, shadow}.Sanitize()
	if err := applyReplicaIdentity(ctx, tx, table, s.ReplicaIdentity); err != nil {
		return err
	}
	if s.Tablespace != "" {
		if _, err := tx.Exec(ctx, "ALTER TABLE "+table+" SET TABLESPACE "+pgx.Identifier{s.Tablespace}.Sanitize()); err != nil {
			return fmt.Errorf("move shadow to tablespace %s: %w", s.Tablespace, err)
		}
	}
	if len(s.RelOptions) > 0 {
		if _, err := tx.Exec(ctx, "ALTER TABLE "+table+" SET ("+strings.Join(s.RelOptions, ", ")+")"); err != nil {
			return fmt.Errorf("set shadow storage parameters: %w", err)
		}
	}
	if s.Comment != "" {
		sql, err := formatSQL(ctx, tx, "COMMENT ON TABLE %s IS %L", table, s.Comment)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, sql); err != nil {
			return fmt.Errorf("set shadow comment: %w", err)
		}
	}
	if s.RLSEnabled {
		if _, err := tx.Exec(ctx, "ALTER TABLE "+table+" ENABLE ROW LEVEL SECURITY"); err != nil {
			return fmt.Errorf("enable shadow row-level security: %w", err)
		}
	}
	if s.RLSForced {
		if _, err := tx.Exec(ctx, "ALTER TABLE "+table+" FORCE ROW LEVEL SECURITY"); err != nil {
			return fmt.Errorf("force shadow row-level security: %w", err)
		}
	}
	if err := syncGrants(ctx, tx, table, shadowOID, s.Grants); err != nil {
		return err
	}
	if err := syncColumnGrants(ctx, tx, table, shadowOID, s.ColumnGrants); err != nil {
		return err
	}
	for _, p := range s.Policies {
		sql, err := policySQL(table, p)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, sql); err != nil {
			return fmt.Errorf("create shadow policy %s: %w", p.Name, err)
		}
	}
	for _, c := range s.UnvalidatedChecks {
		name := pgx.Identifier{c.Name}.Sanitize()
		if _, err := tx.Exec(ctx, "ALTER TABLE "+table+" DROP CONSTRAINT "+name); err != nil {
			return fmt.Errorf("drop validated copy of check constraint %s on shadow: %w", c.Name, err)
		}
		if _, err := tx.Exec(ctx, "ALTER TABLE "+table+" ADD CONSTRAINT "+name+" "+c.Def); err != nil {
			return fmt.Errorf("re-add check constraint %s NOT VALID on shadow: %w", c.Name, err)
		}
	}
	return nil
}

// applyReplicaIdentity carries the source's replica identity onto the shadow.
// DEFAULT is what CREATE TABLE gives, so it needs no statement; FULL is set
// explicitly. Any other value means the proof was not minted by the shape
// check, which admits only these two, so the builder fails closed.
func applyReplicaIdentity(ctx context.Context, tx pgx.Tx, table, identity string) error {
	switch identity {
	case "d":
		return nil
	case "f":
		if _, err := tx.Exec(ctx, "ALTER TABLE "+table+" REPLICA IDENTITY FULL"); err != nil {
			return fmt.Errorf("set shadow replica identity FULL: %w", err)
		}
		return nil
	default:
		// INV: ST-6
		return fmt.Errorf("%w: ST-6: source replica identity %q is outside the copy-and-swap shape the proof admits", ErrInvariantViolation, identity)
	}
}

func policySQL(table string, p Policy) (string, error) {
	command, known := policyCommand(p.Command)
	if !known {
		return "", fmt.Errorf("policy %s carries unknown command code %q", p.Name, p.Command)
	}
	mode := "RESTRICTIVE"
	if p.Permissive {
		mode = "PERMISSIVE"
	}
	roles := make([]string, 0, len(p.Roles)+1)
	if p.AppliesToPublic {
		roles = append(roles, publicKeyword)
	}
	for _, role := range p.Roles {
		roles = append(roles, roleSQL(role, false))
	}
	sql := "CREATE POLICY " + pgx.Identifier{p.Name}.Sanitize() + " ON " + table +
		" AS " + mode + " FOR " + command + " TO " + strings.Join(roles, ", ")
	if p.Using != "" {
		sql += " USING (" + p.Using + ")"
	}
	if p.WithCheck != "" {
		sql += " WITH CHECK (" + p.WithCheck + ")"
	}
	return sql, nil
}

// roleSQL renders a grantee for GRANT, REVOKE, and CREATE POLICY: the PUBLIC
// pseudo-role is the unquoted keyword, and every real role — including one
// that happens to be named "PUBLIC" — is quoted.
func roleSQL(role string, public bool) string {
	if public {
		return publicKeyword
	}
	return pgx.Identifier{role}.Sanitize()
}

// granteeLabel names a grant's recipient for error messages.
func granteeLabel(g Grant) string {
	if g.Public {
		return publicKeyword
	}
	return g.Grantee
}

// formatSQL asks the server to render a statement through format(), so
// literal values (%L) are quoted by PostgreSQL itself rather than by hand.
func formatSQL(ctx context.Context, tx pgx.Tx, template string, args ...string) (string, error) {
	var sql string
	if err := tx.QueryRow(ctx, `SELECT format($1, VARIADIC $2::text[])`, template, args).Scan(&sql); err != nil {
		return "", fmt.Errorf("format statement %q: %w", template, err)
	}
	return sql, nil
}
