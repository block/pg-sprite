package suggest_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/suggest"
)

func TestAdviseCleanScriptHasNoSuggestions(t *testing.T) {
	report, err := suggest.Advise(`
		CREATE TABLE t (id bigint PRIMARY KEY);
		ALTER TABLE t ADD COLUMN age int DEFAULT 0;
		CREATE INDEX CONCURRENTLY t_age_idx ON t (age);
	`)
	require.NoError(t, err)
	assert.Equal(t, suggest.FormatVersion, report.FormatVersion)
	assert.Empty(t, report.Suggestions)
}

func TestAdviseEmptyScriptIsClean(t *testing.T) {
	report, err := suggest.Advise("")
	require.NoError(t, err)
	assert.Empty(t, report.Suggestions)
}

func TestAdviseParseFailureIsError(t *testing.T) {
	_, err := suggest.Advise("ALTER TABEL t ADD COLUMN c int")
	require.Error(t, err)
}

func TestAdviseCreateIndexGetsConcurrentRewrite(t *testing.T) {
	report, err := suggest.Advise("CREATE INDEX t_c_idx ON t (c)")
	require.NoError(t, err)
	require.Len(t, report.Suggestions, 1)
	s := report.Suggestions[0]
	assert.Equal(t, 1, s.Statement)
	assert.Equal(t, planner.ReasonSaferIdiom, s.Reason)
	require.Len(t, s.Recommended, 1)
	assert.NotEqual(t, s.Original, s.Recommended[0], "the recommendation is the concurrent rewrite")
	assert.Equal(t,
		[]suggest.Caveat{suggest.CaveatNonTransactional, suggest.CaveatInvalidIndexOnFailure},
		s.Caveats)
}

func TestAdviseAddCheckGetsNotValidValidateSequence(t *testing.T) {
	report, err := suggest.Advise("ALTER TABLE t ADD CONSTRAINT t_age_pos CHECK (age > 0)")
	require.NoError(t, err)
	require.Len(t, report.Suggestions, 1)
	s := report.Suggestions[0]
	require.Len(t, s.Recommended, 2, "NOT VALID then VALIDATE")
	assert.Equal(t,
		[]suggest.Caveat{suggest.CaveatSeparateTransactions, suggest.CaveatValidationScan},
		s.Caveats)
}

func TestAdviseAddPrimaryKeyGetsUsingIndexSequence(t *testing.T) {
	report, err := suggest.Advise("ALTER TABLE t ADD PRIMARY KEY (id)")
	require.NoError(t, err)
	require.Len(t, report.Suggestions, 1)
	s := report.Suggestions[0]
	require.Len(t, s.Recommended, 2, "concurrent unique index build then USING INDEX attach")
	assert.Equal(t,
		[]suggest.Caveat{suggest.CaveatNonTransactional, suggest.CaveatInvalidIndexOnFailure},
		s.Caveats)
}

func TestAdviseSetNotNullGetsConstraintSequence(t *testing.T) {
	report, err := suggest.Advise("ALTER TABLE t ALTER COLUMN c SET NOT NULL")
	require.NoError(t, err)
	require.Len(t, report.Suggestions, 1)
	s := report.Suggestions[0]
	require.Len(t, s.Recommended, 4, "add NOT VALID, validate, set not null, drop scaffold")
	assert.Equal(t,
		[]suggest.Caveat{suggest.CaveatSeparateTransactions, suggest.CaveatValidationScan},
		s.Caveats)
}

// Statements outside the advisory surface — refusals, table rewrites,
// destructive drops, and forms already safe as written — produce no
// suggestions; they are lint findings, not advice.
func TestAdviseSkipsNonRewritableStatements(t *testing.T) {
	report, err := suggest.Advise(`
		ALTER TABLE t ADD CONSTRAINT no_overlap EXCLUDE USING gist (room WITH =);
		ALTER TABLE t ALTER COLUMN id TYPE bigint;
		ALTER TABLE t DROP COLUMN legacy;
		ALTER TABLE t ADD CONSTRAINT t_fk FOREIGN KEY (o) REFERENCES orders (id) NOT VALID;
	`)
	require.NoError(t, err)
	assert.Empty(t, report.Suggestions)
}

// A multi-operation statement gets no partial rewrite; a rewrite of one
// subcommand of a compound ALTER would be misleading.
func TestAdviseSkipsMultiOperationStatements(t *testing.T) {
	report, err := suggest.Advise(
		"ALTER TABLE t ALTER COLUMN c SET NOT NULL, ADD COLUMN d int")
	require.NoError(t, err)
	assert.Empty(t, report.Suggestions)
}

// Suggestion indexes track the statement position in the script, not the
// suggestion count.
func TestAdviseMultiStatementIndexes(t *testing.T) {
	report, err := suggest.Advise(`
		CREATE TABLE t (id bigint PRIMARY KEY);
		CREATE INDEX t_c_idx ON t (c);
		DROP INDEX t_c_idx;
	`)
	require.NoError(t, err)
	require.Len(t, report.Suggestions, 2)
	assert.Equal(t, 2, report.Suggestions[0].Statement)
	assert.Equal(t, 3, report.Suggestions[1].Statement)
}

// The JSON shape is the automation-facing contract: exact keys, exact
// omissions, suggestions as [] when clean.
func TestReportJSONShape(t *testing.T) {
	report, err := suggest.Advise("CREATE INDEX t_c_idx ON t (c)")
	require.NoError(t, err)
	raw, err := json.Marshal(report)
	require.NoError(t, err)
	require.Len(t, report.Suggestions, 1)
	recommended, err := json.Marshal(report.Suggestions[0].Recommended)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"format_version": 1,
		"suggestions": [
			{
				"statement": 1,
				"original": "CREATE INDEX t_c_idx ON t USING btree (c)",
				"operation": "CREATE INDEX t_c_idx",
				"reason": "safer-idiom",
				"recommended": `+string(recommended)+`,
				"caveats": ["non-transactional", "invalid-index-on-failure"]
			}
		]
	}`, string(raw))
}

func TestReportJSONCleanSuggestionsAreEmptyArray(t *testing.T) {
	report, err := suggest.Advise("CREATE TABLE t (id int)")
	require.NoError(t, err)
	raw, err := json.Marshal(report)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"format_version": 1,
		"suggestions": []
	}`, string(raw))
}
