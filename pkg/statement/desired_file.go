package statement

import (
	"errors"
	"fmt"
	"strings"

	pgquery "github.com/wasilibs/go-pgquery"
)

// DesiredFile is one desired-state schema file admitted through exactly one of
// the three declarative front doors: [ParseDesired] for a table with its
// indexes, [ParseDesiredWithRowSecurity] for a table that also declares row
// security, or the enum type door for a file that declares one enum type
// ([DesiredEnumType]). The choice is made once, by [ParseDesiredFile], so a
// consumer never re-derives which planner owns a file. Only ParseDesiredFile
// constructs a non-zero value, and exactly one of Schema, RowSecurity, and
// EnumType reports ok.
//
// DesiredFile is a router over those proof types, not a proof type itself:
// no planner or executor accepts it. Hand the value Schema, RowSecurity, or
// EnumType returns to the matching planner.
type DesiredFile struct {
	door        desiredDoor
	schema      DesiredSchema
	rowSecurity DesiredWithRowSecurity
	enumType    DesiredEnumType
}

// desiredDoor names the front door that admitted a DesiredFile; the zero
// value admitted nothing.
type desiredDoor int

const (
	doorNone desiredDoor = iota
	doorTable
	doorRowSecurity
	doorEnumType
)

// ParseDesiredFile parses sql once and admits it through the front door its
// statements select. A file that declares an enum type (CREATE TYPE ... AS
// ENUM) must hold that one statement and nothing else ([ErrEnumTypeAlone],
// [ErrEnumLabel], [ErrQualifiedName]). A file that declares row security (a
// policy, a policy comment, or ENABLE, DISABLE, FORCE or NO FORCE ROW LEVEL
// SECURITY) must pass ParseDesiredWithRowSecurity's rules; any other file
// must pass ParseDesired's. Refusals are those front doors' typed errors,
// unchanged; input that is not valid SQL is refused with [ErrParseDesired].
func ParseDesiredFile(sql string) (DesiredFile, error) {
	tree, err := pgquery.Parse(sql)
	if err != nil {
		return DesiredFile{}, fmt.Errorf("%w: %w", ErrParseDesired, err)
	}
	if declaresEnumType(tree) {
		enumType, err := parseDesiredEnumTree(tree)
		if err != nil {
			return DesiredFile{}, err
		}
		return DesiredFile{door: doorEnumType, enumType: enumType}, nil
	}
	if hasRowSecurity(tree) {
		rowSecurity, err := parseDesiredWithRowSecurityTree(sql, tree)
		if err != nil {
			return DesiredFile{}, err
		}
		return DesiredFile{door: doorRowSecurity, rowSecurity: rowSecurity}, nil
	}
	schema, err := parseDesiredTree(sql, tree)
	if err != nil {
		return DesiredFile{}, err
	}
	return DesiredFile{door: doorTable, schema: schema}, nil
}

// FormatDesired returns the canonical text of a desired-state schema file:
// sql is admitted by [ParseDesiredFile] and every admitted statement is
// printed in the deparser's canonical form followed by ";\n", in the order
// [DesiredFile.Statements] reports them, which can differ from the input's
// (indexes after the table, policy comments last).
//
// The file is the one [ParseDesiredFile] admits: exactly one CREATE TABLE
// ([ErrMultipleCreateTables] for a second), unqualified names
// ([ErrQualifiedName]), the table's indexes, and its row-security statements,
// any other statement kind being [ErrDisallowedStatement]; or exactly one
// CREATE TYPE ... AS ENUM and nothing else ([ErrEnumTypeAlone]). Input that is not
// valid SQL, an unterminated quote or comment included, is [ErrParseDesired].
//
// A lexical comment (-- or /* */) is refused with [ErrCommentLoss] before
// any admission rule runs: the parser drops comments, and formatting must
// never silently discard content. An admitted statement such as COMMENT ON
// POLICY is content, not a comment, and is printed. On any refusal the text
// is empty.
//
// The text is stable across calls and formats to itself, but it is the
// canonical form of the PostgreSQL parser this version of pg-sprite builds
// with: it can change when that parser is upgraded, though what the
// statements mean does not.
func FormatDesired(sql string) (string, error) {
	if err := CheckNoComments(sql); err != nil {
		if errors.Is(err, ErrCommentLoss) {
			return "", err
		}
		return "", fmt.Errorf("%w: %w", ErrParseDesired, err)
	}
	file, err := ParseDesiredFile(sql)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for _, st := range file.Statements() {
		out.WriteString(st.SQL())
		out.WriteString(";\n")
	}
	return out.String(), nil
}

// Table returns the unqualified desired table, the same name the admitted
// value's Table reports. An enum type file declares no table, so its Table
// is empty; [DesiredEnumType.Name] names its type.
func (f DesiredFile) Table() string {
	switch f.door {
	case doorTable:
		return f.schema.Table()
	case doorRowSecurity:
		return f.rowSecurity.Table()
	default:
		return ""
	}
}

// Statements returns the admitted statements in the order the admitted
// value's Statements reports them. For a row-security file that is the
// table and its indexes, then the settings and policies in the order the
// file wrote them, then the policy comments. For an enum type file it is
// the one CREATE TYPE. The slice is a copy.
func (f DesiredFile) Statements() []Statement {
	switch f.door {
	case doorTable:
		return f.schema.Statements()
	case doorRowSecurity:
		return f.rowSecurity.Statements()
	case doorEnumType:
		return f.enumType.Statements()
	default:
		return nil
	}
}

// Schema returns the ordinary planner input when the file declares a table
// and no row security. The zero DesiredFile admitted nothing, so it reports
// none of the three.
func (f DesiredFile) Schema() (DesiredSchema, bool) {
	return f.schema, f.door == doorTable
}

// RowSecurity returns the row-security planner input when the file declares
// row security.
func (f DesiredFile) RowSecurity() (DesiredWithRowSecurity, bool) {
	return f.rowSecurity, f.door == doorRowSecurity
}

// EnumType returns the enum type planner input when the file declares an
// enum type.
func (f DesiredFile) EnumType() (DesiredEnumType, bool) {
	return f.enumType, f.door == doorEnumType
}
