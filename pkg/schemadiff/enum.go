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

// EnumType is a live enum type: its schema, its name, and its labels in
// the order PostgreSQL sorts them.
type EnumType struct {
	Schema string
	Name   string
	Labels []string
}

// IntrospectEnumType reads the enum type schema.name and its labels in sort
// order. The name is resolved by explicit qualification, never through the
// search_path. A schema with no type of that name is refused with
// [CauseEnumTypeMissing], and a type of that name that is not an enum (a
// domain, a composite, a range, or a table's row type) with
// [CauseEnumTypeKind]; both are [*UnsupportedChangeError] values, since
// converging either one means creating or replacing a type, which a plan
// never derives.
func IntrospectEnumType(ctx context.Context, db *pgxpool.Pool, schema, name string) (EnumType, error) {
	// INV: CO-9 — every catalog relation, function, operator, and type is
	// pg_catalog-qualified, so a planted object on the session's
	// search_path cannot change which type or labels the read describes.
	const q = `
		SELECT t.typtype OPERATOR(pg_catalog.=) 'e',
		       COALESCE(
		           (SELECT pg_catalog.array_agg(e.enumlabel::pg_catalog.text ORDER BY e.enumsortorder)
		            FROM pg_catalog.pg_enum e
		            WHERE e.enumtypid OPERATOR(pg_catalog.=) t.oid),
		           '{}'::pg_catalog.text[])
		FROM pg_catalog.pg_type t
		JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) t.typnamespace
		WHERE n.nspname OPERATOR(pg_catalog.=) $1
		  AND t.typname OPERATOR(pg_catalog.=) $2`
	qualified := schema + "." + name
	var isEnum bool
	var labels []string
	err := db.QueryRow(ctx, q, schema, name).Scan(&isEnum, &labels)
	if errors.Is(err, pgx.ErrNoRows) {
		return EnumType{}, unsupported(CauseEnumTypeMissing,
			"%w: enum type %s does not exist, and a plan never creates a type; create it with CREATE TYPE, then plan again",
			ErrUnsupportedChange, qualified)
	}
	if err != nil {
		return EnumType{}, fmt.Errorf("introspect enum type %s: %w", qualified, err)
	}
	if !isEnum {
		return EnumType{}, unsupported(CauseEnumTypeKind,
			"%w: %s is a type that is not an enum, and a plan never replaces a type",
			ErrUnsupportedChange, qualified)
	}
	return EnumType{Schema: schema, Name: name, Labels: labels}, nil
}

// DiffEnumType derives the statements that converge the live enum type onto
// the desired one: an ALTER TYPE ... ADD VALUE for every desired label the
// type lacks, in declared order. A label that belongs after every live
// label is appended; any other is placed BEFORE the next desired label the
// type already carries, so adding several labels in one gap keeps their
// declared order. Each statement is schema-qualified, canonical, and kind
// [ChangeAddEnumValue].
//
// PostgreSQL can add an enum label but cannot drop or move one, so the live
// labels must appear in the desired list in the same order. A live label
// the file no longer lists, removed or renamed, is refused with
// [CauseEnumValueRemoved], and live labels the file lists in another order
// with [CauseEnumValueReordered]. An empty result means the type matches.
func DiffEnumType(live EnumType, desired statement.DesiredEnumType) ([]Change, error) {
	if live.Name != desired.Name() {
		return nil, fmt.Errorf("%w: live enum type %q, desired %q", ErrDifferentTypes, live.Name, desired.Name())
	}
	labels := desired.Labels()
	if err := liveLabelsKept(live.Schema+"."+live.Name, live.Labels, labels); err != nil {
		return nil, err
	}
	present := make(map[string]bool, len(live.Labels))
	for _, label := range live.Labels {
		present[label] = true
	}
	var changes []Change
	for i, label := range labels {
		if present[label] {
			continue
		}
		sql := "ALTER TYPE " + pgx.Identifier{live.Schema, live.Name}.Sanitize() + " ADD VALUE " + quoteLiteral(label)
		if next := slices.IndexFunc(labels[i+1:], func(l string) bool { return present[l] }); next >= 0 {
			sql += " BEFORE " + quoteLiteral(labels[i+1+next])
		}
		canonical, err := statement.Canonical(sql)
		if err != nil {
			return nil, fmt.Errorf("canonicalize derived statement %q: %w", sql, err)
		}
		changes = append(changes, Change{SQL: canonical, Kind: ChangeAddEnumValue})
	}
	return changes, nil
}

// liveLabelsKept refuses a desired label list that does not keep every live
// label in its live order: a missing label is a removal (or a rename), and
// every label present but out of order is a reorder.
func liveLabelsKept(qualified string, live, desired []string) error {
	var removed []string
	for _, label := range live {
		if !slices.Contains(desired, label) {
			removed = append(removed, label)
		}
	}
	if len(removed) > 0 {
		return unsupported(CauseEnumValueRemoved,
			"%w: enum type %s carries %s, which the file no longer lists; PostgreSQL cannot drop an enum label, "+
				"so rename it with ALTER TYPE ... RENAME VALUE or keep it in the file",
			ErrUnsupportedChange, qualified, quoteLabels(removed))
	}
	kept := slices.DeleteFunc(slices.Clone(desired), func(l string) bool { return !slices.Contains(live, l) })
	if !slices.Equal(kept, live) {
		return unsupported(CauseEnumValueReordered,
			"%w: enum type %s sorts its labels as %s, and the file lists them as %s; PostgreSQL cannot move an "+
				"enum label, so list them in the live order",
			ErrUnsupportedChange, qualified, quoteLabels(live), quoteLabels(kept))
	}
	return nil
}

// quoteLiteral renders s as an SQL string literal.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// quoteLabels renders labels as a parenthesized list of SQL string
// literals, the way CREATE TYPE ... AS ENUM lists them.
func quoteLabels(labels []string) string {
	quoted := make([]string, len(labels))
	for i, l := range labels {
		quoted[i] = quoteLiteral(l)
	}
	return "(" + strings.Join(quoted, ", ") + ")"
}
