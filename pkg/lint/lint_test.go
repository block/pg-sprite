package lint_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/lint"
	"github.com/block/pg-sprite/pkg/planner"
)

func TestCheckCleanScriptHasNoFindings(t *testing.T) {
	report, err := lint.Check(`
		CREATE TABLE t (id bigint PRIMARY KEY);
		ALTER TABLE t ADD COLUMN age int DEFAULT 0;
		CREATE INDEX CONCURRENTLY t_age_idx ON t (age);
	`)
	require.NoError(t, err)
	assert.Equal(t, lint.FormatVersion, report.FormatVersion)
	assert.Equal(t, planner.RulesPostgresVersions, report.PostgresVersions)
	assert.Empty(t, report.Findings)
	assert.Zero(t, report.Errors)
	assert.Zero(t, report.Warnings)
}

func TestCheckEmptyScriptIsClean(t *testing.T) {
	report, err := lint.Check("")
	require.NoError(t, err)
	assert.Empty(t, report.Findings)
}

func TestCheckParseFailureIsError(t *testing.T) {
	_, err := lint.Check("ALTER TABEL t ADD COLUMN c int")
	require.Error(t, err)
}

func TestCheckFlagsBlockingIdiomWithSuggestion(t *testing.T) {
	report, err := lint.Check("CREATE INDEX t_c_idx ON t (c)")
	require.NoError(t, err)
	require.Len(t, report.Findings, 1)
	f := report.Findings[0]
	assert.Equal(t, 1, f.Statement)
	assert.Equal(t, lint.CodeBlockingIdiom, f.Code)
	assert.Equal(t, lint.SeverityWarning, f.Severity)
	assert.Equal(t, planner.ReasonSaferIdiom, f.Reason)
	require.Len(t, f.Suggestion, 1)
	assert.NotEqual(t, f.SQL, f.Suggestion[0], "the suggestion is the concurrent rewrite")
	assert.Equal(t, 0, report.Errors)
	assert.Equal(t, 1, report.Warnings)
}

// With zero live facts a type change is not provably a rewrite — the
// engine would fail closed to the heavy path, and the finding says that
// rather than asserting a property of the change the linter cannot know.
func TestCheckFlagsUnverifiedTypeChangeAsPossibleRewrite(t *testing.T) {
	report, err := lint.Check("ALTER TABLE t ALTER COLUMN id TYPE bigint")
	require.NoError(t, err)
	require.Len(t, report.Findings, 1)
	f := report.Findings[0]
	assert.Equal(t, lint.CodePossibleTableRewrite, f.Code)
	assert.Equal(t, lint.SeverityWarning, f.Severity)
	assert.Equal(t, planner.ReasonTypeRewrite, f.Reason)
}

// A USING clause is a rewrite regardless of live facts, so the finding is
// the proven code, not the fail-closed one.
func TestCheckFlagsProvenRewriteAsTableRewrite(t *testing.T) {
	report, err := lint.Check("ALTER TABLE t ALTER COLUMN id TYPE integer USING id::integer")
	require.NoError(t, err)
	require.Len(t, report.Findings, 1)
	f := report.Findings[0]
	assert.Equal(t, lint.CodeTableRewrite, f.Code)
	assert.Equal(t, planner.ReasonTypeRewrite, f.Reason)
}

func TestCheckFlagsUnsupportedAsError(t *testing.T) {
	report, err := lint.Check(
		"ALTER TABLE t ADD CONSTRAINT no_overlap EXCLUDE USING gist (room WITH =)")
	require.NoError(t, err, "an unsupported operation is a finding, not a lint failure")
	require.Len(t, report.Findings, 1)
	f := report.Findings[0]
	assert.Equal(t, lint.CodeUnsupportedOperation, f.Code)
	assert.Equal(t, lint.SeverityError, f.Severity)
	assert.Equal(t, planner.ReasonUnsupportedOperation, f.Reason)
	assert.Equal(t, 1, report.Errors)
	assert.Equal(t, 0, report.Warnings)
}

// A column or table rename is metadata-only for PostgreSQL but breaks
// running application code the instant it commits; the finding steers to
// a safe sequence without blocking execution. Index renames stay clean —
// SQL never references an index by name.
func TestCheckFlagsRenamesAsAppBreaking(t *testing.T) {
	report, err := lint.Check(`
		ALTER TABLE t RENAME COLUMN a TO b;
		ALTER TABLE t RENAME TO t2;
		ALTER INDEX i RENAME TO i2;
	`)
	require.NoError(t, err)
	require.Len(t, report.Findings, 2)
	for i, f := range report.Findings {
		assert.Equal(t, i+1, f.Statement)
		assert.Equal(t, lint.CodeAppBreakingRename, f.Code)
		assert.Equal(t, lint.SeverityWarning, f.Severity)
		assert.Equal(t, planner.ReasonAppBreakingRename, f.Reason)
		assert.Empty(t, f.Suggestion)
	}
	assert.Equal(t, 0, report.Errors)
	assert.Equal(t, 2, report.Warnings)
}

// Destructive findings come from the classifier's destructive flag, so
// the linter and the plan report mark the same operations by
// construction. Index drops are destructive even in the concurrent form:
// offline the linter cannot see whether the index is unique, and a
// dropped unique index is not recreatable once writes exploit the gap.
func TestCheckFlagsDestructiveDrops(t *testing.T) {
	report, err := lint.Check(`
		ALTER TABLE t DROP COLUMN legacy;
		ALTER TABLE t DROP CONSTRAINT t_check;
		DROP INDEX CONCURRENTLY t_c_idx;
	`)
	require.NoError(t, err)
	var codes []lint.Code
	var stmts []int
	for _, f := range report.Findings {
		codes = append(codes, f.Code)
		stmts = append(stmts, f.Statement)
	}
	assert.Equal(t, []lint.Code{
		lint.CodeDestructive, lint.CodeDestructive, lint.CodeDestructive,
	}, codes)
	assert.Equal(t, []int{1, 2, 3}, stmts)
}

// A multi-operation statement can produce several findings, and finding
// indexes track the statement, not the finding count.
func TestCheckMultiStatementMultiFinding(t *testing.T) {
	report, err := lint.Check(`
		ALTER TABLE t DROP COLUMN legacy, ALTER COLUMN id TYPE bigint;
		CREATE INDEX t_c_idx ON t (c);
	`)
	require.NoError(t, err)
	require.Len(t, report.Findings, 3)
	assert.Equal(t, lint.CodeDestructive, report.Findings[0].Code)
	assert.Equal(t, 1, report.Findings[0].Statement)
	assert.Equal(t, lint.CodePossibleTableRewrite, report.Findings[1].Code)
	assert.Equal(t, 1, report.Findings[1].Statement)
	assert.Equal(t, lint.CodeBlockingIdiom, report.Findings[2].Code)
	assert.Equal(t, 2, report.Findings[2].Statement)
	assert.Equal(t, 0, report.Errors)
	assert.Equal(t, 3, report.Warnings)
}

// Findings carry the statement's verbatim source text and its position,
// so a CI system can annotate the file and a reader can find the text by
// exact match.
func TestCheckFindingsCarrySourcePositions(t *testing.T) {
	report, err := lint.Check(
		"CREATE TABLE ok (id int);\n\n\n\n\nALTER TABLE t DROP COLUMN legacy_a;\n")
	require.NoError(t, err)
	require.Len(t, report.Findings, 1)
	f := report.Findings[0]
	assert.Equal(t, 2, f.Statement)
	assert.Equal(t, 6, f.Line)
	assert.Equal(t, 1, f.Column)
	assert.Equal(t, "ALTER TABLE t DROP COLUMN legacy_a", f.SQL,
		"verbatim source, not a canonical reprint")
}

// The JSON shape is the automation-facing contract: exact keys, exact
// omissions, findings as [] when clean.
func TestReportJSONShape(t *testing.T) {
	report, err := lint.Check("ALTER TABLE t ALTER COLUMN c SET NOT NULL")
	require.NoError(t, err)
	raw, err := json.Marshal(report)
	require.NoError(t, err)
	require.Len(t, report.Findings, 1)
	suggestion, err := json.Marshal(report.Findings[0].Suggestion)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"format_version": 1,
		"postgres_versions": "14-18",
		"findings": [
			{
				"statement": 1,
				"line": 1,
				"column": 1,
				"sql": "ALTER TABLE t ALTER COLUMN c SET NOT NULL",
				"operation": "ALTER COLUMN c SET NOT NULL",
				"code": "blocking-idiom",
				"severity": "warning",
				"reason": "safer-idiom",
				"suggestion": `+string(suggestion)+`
			}
		],
		"errors": 0,
		"warnings": 1
	}`, string(raw))
}

func TestReportJSONCleanFindingsAreEmptyArray(t *testing.T) {
	report, err := lint.Check("CREATE TABLE t (id int)")
	require.NoError(t, err)
	raw, err := json.Marshal(report)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"format_version": 1,
		"postgres_versions": "14-18",
		"findings": [],
		"errors": 0,
		"warnings": 0
	}`, string(raw))
}
