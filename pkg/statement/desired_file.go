package statement

import (
	"errors"
	"fmt"
	"strings"

	pgquery "github.com/wasilibs/go-pgquery"
)

// DesiredFile is one desired-state schema file admitted through exactly one of
// the two declarative front doors: [ParseDesired] for a table with its indexes,
// or [ParseDesiredWithRowSecurity] for a table that also declares row security.
// The choice is made once, by [ParseDesiredFile], so a consumer never
// re-derives which planner owns a file. Only ParseDesiredFile constructs a
// non-zero value, and exactly one of Schema and RowSecurity reports ok.
//
// DesiredFile is a router over those two proof types, not a proof type itself:
// no planner or executor accepts it. Hand the value Schema or RowSecurity
// returns to the matching planner.
type DesiredFile struct {
	schema      DesiredSchema
	rowSecurity DesiredWithRowSecurity
	hasRLS      bool
}

// ParseDesiredFile parses sql once and admits it through the front door its
// statements select. A file that declares row security (a policy, a policy
// comment, or ENABLE, DISABLE, FORCE or NO FORCE ROW LEVEL SECURITY) must pass
// ParseDesiredWithRowSecurity's rules; any other file must pass ParseDesired's.
// Refusals are those front doors' typed errors, unchanged; input that is not
// valid SQL is refused with [ErrParseDesired].
func ParseDesiredFile(sql string) (DesiredFile, error) {
	tree, err := pgquery.Parse(sql)
	if err != nil {
		return DesiredFile{}, fmt.Errorf("%w: %w", ErrParseDesired, err)
	}
	if hasRowSecurity(tree) {
		rowSecurity, err := parseDesiredWithRowSecurityTree(sql, tree)
		if err != nil {
			return DesiredFile{}, err
		}
		return DesiredFile{rowSecurity: rowSecurity, hasRLS: true}, nil
	}
	schema, err := parseDesiredTree(sql, tree)
	if err != nil {
		return DesiredFile{}, err
	}
	return DesiredFile{schema: schema}, nil
}

// FormatDesired returns the canonical text of a desired-state schema file:
// sql is admitted by [ParseDesiredFile] and every admitted statement is
// printed in the deparser's canonical form followed by ";\n", in the order
// [DesiredFile.Statements] reports them, which can differ from the input's
// (indexes after the table, policy comments last).
//
// The file is the one [ParseDesiredFile] admits: exactly one CREATE TABLE
// ([ErrMultipleCreateTables] for a second), unqualified names
// ([ErrQualifiedName]), the table's indexes, and its row-security statements;
// any other statement kind is [ErrDisallowedStatement]. Input that is not
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
// value's Table reports.
func (f DesiredFile) Table() string {
	if f.hasRLS {
		return f.rowSecurity.Table()
	}
	return f.schema.Table()
}

// Statements returns the admitted statements in the order the admitted
// value's Statements reports them. For a row-security file that is the
// table and its indexes, then the settings and policies in the order the
// file wrote them, then the policy comments. The slice is a copy.
func (f DesiredFile) Statements() []Statement {
	if f.hasRLS {
		return f.rowSecurity.Statements()
	}
	return f.schema.Statements()
}

// Schema returns the ordinary planner input when the file declares no row
// security. The zero DesiredFile admitted nothing, so it reports neither.
func (f DesiredFile) Schema() (DesiredSchema, bool) {
	// The table check alone suffices for values ParseDesiredFile builds, which
	// leave schema zero on the row-security branch; !hasRLS keeps the
	// "exactly one reports ok" guarantee local to this type.
	return f.schema, !f.hasRLS && f.schema.Table() != ""
}

// RowSecurity returns the row-security planner input when the file declares
// row security.
func (f DesiredFile) RowSecurity() (DesiredWithRowSecurity, bool) {
	return f.rowSecurity, f.hasRLS
}
