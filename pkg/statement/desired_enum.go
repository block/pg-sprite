package statement

import (
	"errors"
	"fmt"
	"slices"

	pganalyze "github.com/pganalyze/pg_query_go/v6"
)

// Typed refusals for a desired enum type file. The caller branches with
// errors.Is, never on text.
var (
	// ErrEnumTypeAlone is returned for a statement in a desired enum type
	// file other than its one CREATE TYPE ... AS ENUM: the file declares one
	// type, and a table or a second type belongs in a file of its own. The
	// parser returns it inside an [*EnumTypeFileError], which names the
	// refused statement.
	ErrEnumTypeAlone = errors.New("a desired enum type file holds exactly one CREATE TYPE ... AS ENUM and nothing else")
	// ErrEnumLabel is returned when a desired enum type lists a label twice,
	// lists the empty label, or lists one longer than PostgreSQL stores: a
	// plan places each new label next to a neighbor it names, so every label
	// must be one the plan can name and the server can add.
	ErrEnumLabel = errors.New("a desired enum type's labels must be distinct, non-empty, and at most 63 bytes")
)

// EnumTypeFileError is [ErrEnumTypeAlone] for one statement of a desired
// enum type file.
type EnumTypeFileError struct {
	// SQL is the refused statement as [DisallowedStatementError.SQL]
	// names it: the canonical form, or the source text when the canonical
	// form would classify as a different kind.
	SQL string
}

func (e *EnumTypeFileError) Error() string {
	return fmt.Sprintf("%v: %s", ErrEnumTypeAlone, e.SQL)
}

// Unwrap returns [ErrEnumTypeAlone].
func (e *EnumTypeFileError) Unwrap() error { return ErrEnumTypeAlone }

// DesiredEnumType proves a desired-state schema file declares exactly one
// enum type: a single unqualified CREATE TYPE ... AS ENUM whose labels are
// distinct, non-empty, and at most 63 bytes. Only [ParseDesiredFile] produces a non-zero
// value.
//
// The labels are the type's desired values in their declared order, which
// is the order PostgreSQL sorts them in. A plan converges a live type onto
// them by adding the labels it lacks, each in its declared position; it
// never removes, renames, or reorders a label.
type DesiredEnumType struct {
	name      string
	labels    []string
	statement Statement
}

// Name returns the unqualified name of the enum type.
func (d DesiredEnumType) Name() string { return d.name }

// Labels returns the desired labels in their declared order. The slice is
// a copy.
func (d DesiredEnumType) Labels() []string { return slices.Clone(d.labels) }

// Statements returns the file's one CREATE TYPE ... AS ENUM in the
// deparser's canonical form. The slice is a copy.
func (d DesiredEnumType) Statements() []Statement { return []Statement{d.statement} }

// declaresEnumType reports whether a parsed file selects the enum type
// front door: some statement is a CREATE TYPE ... AS ENUM and none is a
// CREATE TABLE. A table file that also holds a type stays a table file, so
// its refusal names the CREATE TYPE, with the class the gate gives catalog
// work, rather than naming the table.
func declaresEnumType(tree *pganalyze.ParseResult) bool {
	return slices.ContainsFunc(tree.GetStmts(), func(raw *pganalyze.RawStmt) bool {
		return raw.GetStmt().GetCreateEnumStmt() != nil
	}) && !slices.ContainsFunc(tree.GetStmts(), func(raw *pganalyze.RawStmt) bool {
		return raw.GetStmt().GetCreateStmt() != nil
	})
}

// parseDesiredEnumTree admits tree, the parse of a source file that
// declares an enum type: the file's only statement is one unqualified
// CREATE TYPE ... AS ENUM with distinct, non-empty labels.
func parseDesiredEnumTree(source string, tree *pganalyze.ParseResult) (DesiredEnumType, error) {
	var desired DesiredEnumType
	refused := newRefusalCollector(source)
	declared := false
	for i, raw := range tree.GetStmts() {
		node := raw.GetStmt()
		create := node.GetCreateEnumStmt()
		if create == nil || declared {
			if err := refused.refuse(i, raw, enumTypeAloneRefusal(source, raw)); err != nil {
				return DesiredEnumType{}, err
			}
			continue
		}
		declared = true
		name, err := enumTypeName(create)
		if err != nil {
			if err := refused.refuse(i, raw, err); err != nil {
				return DesiredEnumType{}, err
			}
			continue
		}
		labels, err := enumLabels(create)
		if err != nil {
			if err := refused.refuse(i, raw, err); err != nil {
				return DesiredEnumType{}, err
			}
			continue
		}
		sql, err := deparseOne(node)
		if err != nil {
			return DesiredEnumType{}, fmt.Errorf("statement %d: %w", i+1, err)
		}
		st, err := ParseOne(sql)
		if err != nil {
			return DesiredEnumType{}, fmt.Errorf("statement %d: classify %s: %w", i+1, sql, err)
		}
		desired = DesiredEnumType{name: name, labels: labels, statement: st}
	}
	if err := refused.err(); err != nil {
		return DesiredEnumType{}, err
	}
	return desired, nil
}

// enumTypeAloneRefusal returns the refusal for raw, a statement of an enum
// type file other than its one CREATE TYPE ... AS ENUM.
func enumTypeAloneRefusal(source string, raw *pganalyze.RawStmt) error {
	sql, _, err := refusedStatement(source, raw)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrEnumTypeAlone, err)
	}
	return &EnumTypeFileError{SQL: sql}
}

// enumTypeName returns the unqualified type name a CREATE TYPE ... AS ENUM
// declares, refusing a qualified one as every desired file does.
func enumTypeName(create *pganalyze.CreateEnumStmt) (string, error) {
	parts := make([]string, 0, len(create.GetTypeName()))
	for _, part := range create.GetTypeName() {
		parts = append(parts, part.GetString_().GetSval())
	}
	switch len(parts) {
	case 1:
		return parts[0], nil
	case 2:
		return "", fmt.Errorf("%w: %s.%s", ErrQualifiedName, parts[0], parts[1])
	default:
		return "", fmt.Errorf("%w: a type name of %d parts", ErrQualifiedName, len(parts))
	}
}

// maxEnumLabelBytes is PostgreSQL's NAMEDATALEN - 1: an enum label is
// stored as a name, and the server refuses a longer one when it is added.
const maxEnumLabelBytes = 63

// enumLabels returns the labels of a CREATE TYPE ... AS ENUM in declared
// order, refusing a duplicate, the empty label, or a label longer than the
// server stores, so a plan never commits some labels and then fails on one
// it could have refused before running anything.
func enumLabels(create *pganalyze.CreateEnumStmt) ([]string, error) {
	labels := make([]string, 0, len(create.GetVals()))
	seen := make(map[string]bool, len(create.GetVals()))
	for _, val := range create.GetVals() {
		label := val.GetString_().GetSval()
		switch {
		case label == "":
			return nil, fmt.Errorf("%w: the empty label", ErrEnumLabel)
		case len(label) > maxEnumLabelBytes:
			return nil, fmt.Errorf("%w: %q is %d bytes", ErrEnumLabel, label, len(label))
		case seen[label]:
			return nil, fmt.Errorf("%w: %q is listed twice", ErrEnumLabel, label)
		}
		seen[label] = true
		labels = append(labels, label)
	}
	return labels, nil
}
