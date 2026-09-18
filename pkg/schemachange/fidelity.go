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
	// RelOptions are the table's storage parameters as the server prints
	// them, with the TOAST relation's parameters prefixed "toast.".
	RelOptions []string
	// Grants are the table's privileges, expanded from its ACL (or from the
	// owner's default ACL when none is stored).
	Grants []Grant
	// Policies are the table's row-level-security policies.
	Policies []Policy
	// UnvalidatedChecks are the CHECK constraints the user left NOT VALID.
	// LIKE copies them as validated; the builder re-adds them NOT VALID so
	// the copier accepts every row the source legally holds.
	UnvalidatedChecks []UnvalidatedConstraint
}

// Grant is one expanded ACL entry.
type Grant struct {
	// Privilege is the privilege keyword as aclexplode reports it.
	Privilege string
	// Grantee is the role name, or PublicRole for the PUBLIC pseudo-role.
	Grantee string
	// Grantable reports WITH GRANT OPTION.
	Grantable bool
}

// Policy is one row-level-security policy.
type Policy struct {
	// Name is the policy name.
	Name string
	// Permissive reports AS PERMISSIVE (false is AS RESTRICTIVE).
	Permissive bool
	// Command is the pg_policy.polcmd code: '*', 'r', 'a', 'w', or 'd'.
	Command string
	// Roles are the role names the policy applies to; PublicRole for PUBLIC.
	Roles []string
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

// PublicRole is the spelling the snapshot uses for the PUBLIC pseudo-role
// (grantee OID 0). PostgreSQL reserves "public" as a role name, so no real
// role can collide with it.
const PublicRole = "PUBLIC"

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
		       COALESCE(c.reloptions, '{}'),
		       COALESCE((SELECT t.reloptions FROM pg_class t WHERE t.oid = c.reltoastrelid), '{}')
		FROM pg_class c
		WHERE c.oid = $1`, oid).
		Scan(&s.Owner, &s.ReplicaIdentity, &s.RLSEnabled, &s.RLSForced, &s.Comment, &s.RelOptions, &toastOptions)
	if err != nil {
		return FidelitySnapshot{}, fmt.Errorf("read table metadata: %w", err)
	}
	for _, opt := range toastOptions {
		s.RelOptions = append(s.RelOptions, "toast."+opt)
	}
	if s.Grants, err = readGrants(ctx, tx, oid); err != nil {
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

func readGrants(ctx context.Context, tx pgx.Tx, oid uint32) ([]Grant, error) {
	rows, err := tx.Query(ctx, `
		SELECT a.privilege_type,
		       CASE WHEN a.grantee = 0 THEN $2 ELSE pg_get_userbyid(a.grantee) END,
		       a.is_grantable
		FROM pg_class c
		CROSS JOIN LATERAL aclexplode(COALESCE(c.relacl, acldefault('r', c.relowner))) a
		WHERE c.oid = $1
		ORDER BY 2, 1`, oid, PublicRole)
	if err != nil {
		return nil, fmt.Errorf("read table grants: %w", err)
	}
	grants, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Grant, error) {
		var g Grant
		err := row.Scan(&g.Privilege, &g.Grantee, &g.Grantable)
		return g, err
	})
	if err != nil {
		return nil, fmt.Errorf("read table grants: %w", err)
	}
	for _, g := range grants {
		if !isTablePrivilege(g.Privilege) {
			return nil, fmt.Errorf("table grant to %s carries unknown privilege %q", g.Grantee, g.Privilege)
		}
	}
	return grants, nil
}

func readPolicies(ctx context.Context, tx pgx.Tx, oid uint32) ([]Policy, error) {
	rows, err := tx.Query(ctx, `
		SELECT p.polname, p.polpermissive, p.polcmd::text,
		       ARRAY(SELECT CASE WHEN r = 0 THEN $2 ELSE pg_get_userbyid(r) END
		             FROM unnest(p.polroles) r),
		       COALESCE(pg_get_expr(p.polqual, p.polrelid), ''),
		       COALESCE(pg_get_expr(p.polwithcheck, p.polrelid), '')
		FROM pg_policy p
		WHERE p.polrelid = $1
		ORDER BY p.polname`, oid, PublicRole)
	if err != nil {
		return nil, fmt.Errorf("read row-level-security policies: %w", err)
	}
	policies, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Policy, error) {
		var p Policy
		err := row.Scan(&p.Name, &p.Permissive, &p.Command, &p.Roles, &p.Using, &p.WithCheck)
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
// SET ROLE owner, so it is owner-correct from birth and the builder verifies
// that instead. Every role and relation name goes through Sanitize; the
// comment goes through the server's format(%L) so no literal is spliced by
// hand; expressions, options, and constraint definitions are the server's
// own decompiled text.
func applyFidelity(ctx context.Context, tx pgx.Tx, schema, shadow string, s FidelitySnapshot) error {
	table := pgx.Identifier{schema, shadow}.Sanitize()
	if err := applyReplicaIdentity(ctx, tx, table, s.ReplicaIdentity); err != nil {
		return err
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
	for _, g := range s.Grants {
		if _, err := tx.Exec(ctx, grantSQL(table, g)); err != nil {
			return fmt.Errorf("grant %s on shadow to %s: %w", g.Privilege, g.Grantee, err)
		}
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

func grantSQL(table string, g Grant) string {
	sql := "GRANT " + g.Privilege + " ON TABLE " + table + " TO " + roleSQL(g.Grantee)
	if g.Grantable {
		sql += " WITH GRANT OPTION"
	}
	return sql
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
	roles := make([]string, len(p.Roles))
	for i, role := range p.Roles {
		roles[i] = roleSQL(role)
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

// roleSQL renders a role name for GRANT and CREATE POLICY: the PUBLIC
// pseudo-role is a keyword and must not be quoted; every real role is.
func roleSQL(role string) string {
	if role == PublicRole {
		return PublicRole
	}
	return pgx.Identifier{role}.Sanitize()
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
