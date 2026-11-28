package schemadiff

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/statement"
)

// DiffWithRowSecurityOnDesired is DiffWithRowSecurity for a file whose
// table definition may change the types its policies read. PostgreSQL
// stores a policy's expressions as it resolved them against the table's
// column types, so a file that keeps a policy's text and changes a column
// it reads, such as text to varchar, prints that policy differently from
// the live one: an implicit cast appears where none was. When the two
// models' policies differ only in the text of their expressions, the live
// policies are created again on the file's table definition in a
// rolled-back scratch transaction and compared as PostgreSQL prints them
// there. A policy the file does not change then matches, and the result is
// the table difference on its own.
//
// The live policies that cannot be created on the file's definition, such
// as one that reads a column the file retypes to a type the expression does
// not accept, do not carry over unchanged, so the row security difference
// stands, and the error says why the live policies could not be compared.
// desired is the file wanted was introspected from.
func DiffWithRowSecurityOnDesired(ctx context.Context, db *pgxpool.Pool, schema string, live, wanted Model, desired statement.DesiredWithRowSecurity) ([]Change, error) {
	changes, err := DiffWithRowSecurity(schema, live, wanted)
	if err == nil || errors.Is(err, ErrOnlyTableDiffers) || !policiesDifferOnlyInExpressions(live.RowSecurity, wanted.RowSecurity) {
		return changes, err
	}
	security, renderErr := liveRowSecurityOnDesired(ctx, db, schema, live, desired)
	if renderErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("compare the live policies of %q on the file's table definition: %w", live.Table, ctxErr)
		}
		return nil, fmt.Errorf("%w (the live policies of %q could not be created on the file's table definition to compare them: %w)", err, live.Table, renderErr)
	}
	live.RowSecurity = security
	return DiffWithRowSecurity(schema, live, wanted)
}

// policiesDifferOnlyInExpressions reports whether a and b declare the same
// settings and the same policies, each with the same name, command, mode,
// roles, comment, and clauses, so that only the text of a USING or WITH
// CHECK expression can tell them apart.
func policiesDifferOnlyInExpressions(a, b RowSecurity) bool {
	if a.Enabled != b.Enabled || a.Forced != b.Forced {
		return false
	}
	return slices.EqualFunc(a.Policies, b.Policies, func(p, q Policy) bool {
		return p.Name == q.Name && p.Command == q.Command && p.Permissive == q.Permissive &&
			slices.Equal(p.Roles, q.Roles) && optionalTextEqual(p.Comment, q.Comment) &&
			(p.Using == nil) == (q.Using == nil) && (p.WithCheck == nil) == (q.WithCheck == nil)
	})
}

// liveRowSecurityOnDesired returns live's row security as PostgreSQL prints
// it once its policies are created on desired's table definition: the file's
// CREATE TABLE and indexes, followed by live's settings and policies, built
// and read in a rolled-back scratch transaction as the file itself is. The
// live policies are printed with every name outside pg_catalog qualified, so
// they pass the file's own admission rules.
func liveRowSecurityOnDesired(ctx context.Context, db *pgxpool.Pool, schema string, live Model, desired statement.DesiredWithRowSecurity) (RowSecurity, error) {
	var b strings.Builder
	for _, st := range desired.TableDefinition().Statements() {
		b.WriteString(st.SQL())
		b.WriteString(";\n")
	}
	if err := renderRowSecurity(&b, pgx.Identifier{desired.Table()}.Sanitize(), live.RowSecurity); err != nil {
		return RowSecurity{}, fmt.Errorf("render the live row security of %q: %w", live.Table, err)
	}
	carried, err := statement.ParseDesiredWithRowSecurity(b.String())
	if err != nil {
		return RowSecurity{}, fmt.Errorf("admit the live row security of %q on the file's table definition: %w", live.Table, err)
	}
	if carried, err = carried.WithExtensionSchemas(desired.ExtensionSchemas()...); err != nil {
		return RowSecurity{}, fmt.Errorf("resolve the file's extension schemas for %q: %w", live.Table, err)
	}
	m, err := IntrospectDesiredWithRowSecurity(ctx, db, schema, carried)
	if err != nil {
		return RowSecurity{}, err
	}
	return m.RowSecurity, nil
}
