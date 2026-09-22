package schemadiff

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/statement"
)

// IntrospectDesiredWithRowSecurity materializes an explicitly RLS-scoped file in
// a rolled-back scratch transaction. Roles and qualified helpers must exist;
// this never provisions authentication or changes the live table.
func IntrospectDesiredWithRowSecurity(ctx context.Context, db *pgxpool.Pool, desired statement.DesiredWithRowSecurity) (Model, error) {
	if desired.Table() == "" {
		return Model{}, statement.ErrRowSecurityDeclaration
	}
	return introspectDesiredStatements(ctx, db, desired.Table(), desired.Statements())
}

// IntrospectDesiredWithRowSecurityTx materializes the declaration in a nested
// transaction (savepoint) and always rolls it back. The parent's locks survive.
func IntrospectDesiredWithRowSecurityTx(ctx context.Context, tx pgx.Tx, desired statement.DesiredWithRowSecurity) (Model, error) {
	if desired.Table() == "" {
		return Model{}, statement.ErrRowSecurityDeclaration
	}
	scratch, err := tx.Begin(ctx)
	if err != nil {
		return Model{}, fmt.Errorf("begin desired row security savepoint: %w", err)
	}
	return introspectDesiredTransaction(ctx, scratch, desired.Table(), desired.Statements())
}

// RenderWithRowSecurity exports the complete table-local RLS definition,
// including explicit DISABLE when RLS is off. The result is admitted only by
// ParseDesiredWithRowSecurity and cannot be passed to the live create executor.
func RenderWithRowSecurity(m Model) (string, error) {
	table := m
	table.RowSecurity = RowSecurity{}
	base, err := Render(table)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(base)
	target := pgx.Identifier{m.Table}.Sanitize()
	state := "DISABLE"
	if m.RowSecurity.Enabled {
		state = "ENABLE"
	}
	fmt.Fprintf(&b, "\nALTER TABLE %s %s ROW LEVEL SECURITY;\n", target, state)
	force := "NO FORCE"
	if m.RowSecurity.Forced {
		force = "FORCE"
	}
	fmt.Fprintf(&b, "ALTER TABLE %s %s ROW LEVEL SECURITY;\n", target, force)
	for _, policy := range m.RowSecurity.Policies {
		if err := renderPolicy(&b, target, policy); err != nil {
			return "", err
		}
	}
	out := b.String()
	if _, err := statement.ParseDesiredWithRowSecurity(out); err != nil {
		return "", fmt.Errorf("render row security for %q: %w", m.Table, err)
	}
	return out, nil
}

func renderPolicy(b *strings.Builder, target string, p Policy) error {
	var command string
	switch p.Command {
	case PolicyAll:
		command = "ALL"
	case PolicySelect:
		command = "SELECT"
	case PolicyInsert:
		command = "INSERT"
	case PolicyUpdate:
		command = "UPDATE"
	case PolicyDelete:
		command = "DELETE"
	default:
		return fmt.Errorf("policy %q command %q: %w", p.Name, p.Command, ErrUnrenderableRowSecurity)
	}
	if len(p.Roles) == 0 {
		return fmt.Errorf("policy %q has no roles: %w", p.Name, ErrUnrenderableRowSecurity)
	}
	mode := "RESTRICTIVE"
	if p.Permissive {
		mode = "PERMISSIVE"
	}
	roles := make([]string, len(p.Roles))
	for i, role := range p.Roles {
		if role == "public" {
			roles[i] = "PUBLIC"
		} else {
			roles[i] = pgx.Identifier{role}.Sanitize()
		}
	}
	name := pgx.Identifier{p.Name}.Sanitize()
	fmt.Fprintf(b, "\nCREATE POLICY %s ON %s\n    AS %s FOR %s TO %s", name, target, mode, command, strings.Join(roles, ", "))
	if p.Using != nil {
		fmt.Fprintf(b, "\n    USING (%s)", *p.Using)
	}
	if p.WithCheck != nil {
		fmt.Fprintf(b, "\n    WITH CHECK (%s)", *p.WithCheck)
	}
	b.WriteString(";\n")
	if p.Comment != nil {
		// E-string quoting preserves both quotes and backslashes regardless of
		// standard_conforming_strings. Names use identifier quoting above.
		comment := strings.ReplaceAll(*p.Comment, `\`, `\\`)
		comment = strings.ReplaceAll(comment, "'", "''")
		fmt.Fprintf(b, "COMMENT ON POLICY %s ON %s IS E'%s';\n", name, target, comment)
	}
	return nil
}

// DiffWithRowSecurity checks equality of a complete RLS-scoped declaration.
// Security deltas and mixed table/security changes return ErrUnsupportedChange;
// this comparison never emits executable policy SQL. The dedicated atomic
// executor uses it to verify convergence. Table-only callers retain Diff.
func DiffWithRowSecurity(schema string, live, desired Model) ([]Change, error) {
	if live.Table != desired.Table {
		return nil, ErrDifferentTables
	}
	if !rowSecurityEqual(live.RowSecurity, desired.RowSecurity) {
		return nil, fmt.Errorf("row security differs; use the dedicated atomic RLS executor: %w", ErrUnsupportedChange)
	}
	live.RowSecurity = RowSecurity{}
	desired.RowSecurity = RowSecurity{}
	changes, err := Diff(schema, live, desired)
	if err != nil {
		return nil, err
	}
	if len(changes) != 0 {
		return nil, fmt.Errorf("table changes with managed row security are not supported: %w", ErrUnsupportedChange)
	}
	return changes, nil
}

func rowSecurityEqual(a, b RowSecurity) bool {
	if a.Enabled != b.Enabled || a.Forced != b.Forced {
		return false
	}
	return slices.EqualFunc(a.Policies, b.Policies, policyEqual)
}

func policyEqual(a, b Policy) bool {
	if a.Name != b.Name || a.Command != b.Command {
		return false
	}
	if a.Permissive != b.Permissive {
		return false
	}
	if !slices.Equal(a.Roles, b.Roles) {
		return false
	}
	if !optionalTextEqual(a.Using, b.Using) {
		return false
	}
	if !optionalTextEqual(a.WithCheck, b.WithCheck) {
		return false
	}
	return optionalTextEqual(a.Comment, b.Comment)
}

func optionalTextEqual(a, b *string) bool {
	if a == nil {
		return b == nil
	}
	if b == nil {
		return false
	}
	return *a == *b
}
