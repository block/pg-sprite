package suggest_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/lint"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/statement"
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
	assert.Equal(t, planner.ExecutionAutocommit, s.Execution)
	assert.Equal(t,
		[]suggest.Caveat{suggest.CaveatNonTransactional, suggest.CaveatInvalidIndexOnFailure},
		s.Caveats)
	assert.Empty(t, s.Guidance, "a constructed rewrite carries no guidance")
}

func TestAdviseAddCheckGetsNotValidValidateSequence(t *testing.T) {
	report, err := suggest.Advise("ALTER TABLE t ADD CONSTRAINT t_age_pos CHECK (age > 0)")
	require.NoError(t, err)
	require.Len(t, report.Suggestions, 1)
	s := report.Suggestions[0]
	require.Len(t, s.Recommended, 2, "NOT VALID then VALIDATE")
	assert.Equal(t, planner.ExecutionAutocommit, s.Execution)
	assert.Equal(t,
		[]suggest.Caveat{suggest.CaveatSeparateTransactions, suggest.CaveatValidationScan,
			suggest.CaveatScaffoldConstraintOnFailure},
		s.Caveats)
}

func TestAdviseAddPrimaryKeyGetsUsingIndexSequence(t *testing.T) {
	report, err := suggest.Advise("ALTER TABLE t ADD PRIMARY KEY (id)")
	require.NoError(t, err)
	require.Len(t, report.Suggestions, 1)
	s := report.Suggestions[0]
	require.Len(t, s.Recommended, 2, "concurrent unique index build then USING INDEX attach")
	assert.Equal(t, planner.ExecutionAutocommit, s.Execution)
	assert.Equal(t,
		[]suggest.Caveat{suggest.CaveatNonTransactional, suggest.CaveatSeparateTransactions,
			suggest.CaveatInvalidIndexOnFailure},
		s.Caveats)
}

func TestAdviseSetNotNullGetsConstraintSequence(t *testing.T) {
	report, err := suggest.Advise("ALTER TABLE t ALTER COLUMN c SET NOT NULL")
	require.NoError(t, err)
	require.Len(t, report.Suggestions, 1)
	s := report.Suggestions[0]
	require.Len(t, s.Recommended, 4, "add NOT VALID, validate, set not null, drop scaffold")
	assert.Equal(t, planner.ExecutionAutocommit, s.Execution)
	assert.Equal(t,
		[]suggest.Caveat{suggest.CaveatSeparateTransactions, suggest.CaveatValidationScan,
			suggest.CaveatScaffoldConstraintOnFailure},
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

// A multi-operation statement gets no partial rewrite — a rewrite of one
// subcommand of a compound ALTER would be misleading — but it is not
// silent: each risky operation carries split-statement guidance.
func TestAdviseMultiOperationStatementsGetSplitGuidance(t *testing.T) {
	report, err := suggest.Advise(
		"ALTER TABLE t ALTER COLUMN c SET NOT NULL, ADD COLUMN d int")
	require.NoError(t, err)
	require.Len(t, report.Suggestions, 1, "only the risky operation gets advice")
	s := report.Suggestions[0]
	assert.Equal(t, planner.ReasonSaferIdiom, s.Reason)
	assert.Empty(t, s.Recommended)
	assert.Empty(t, s.Execution)
	assert.Empty(t, s.Caveats)
	assert.Equal(t, suggest.GuidanceSplitStatement, s.Guidance)
}

// ATTACH PARTITION has a safer native pattern the planner cannot construct
// (the proving CHECK depends on the partition bound); the suggestion names
// the manual path instead of staying silent.
func TestAdviseAttachPartitionGetsPrevalidatedCheckGuidance(t *testing.T) {
	report, err := suggest.Advise(
		"ALTER TABLE orders ATTACH PARTITION orders_2026 FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')")
	require.NoError(t, err)
	require.Len(t, report.Suggestions, 1)
	s := report.Suggestions[0]
	assert.Empty(t, s.Recommended)
	assert.Equal(t, suggest.GuidancePrevalidatedCheck, s.Guidance)
}

// An inline constraint on ADD COLUMN builds under the ADD COLUMN's ACCESS
// EXCLUSIVE lock; the advice is to split the column addition from an
// online constraint build.
func TestAdviseInlineConstraintGetsAddColumnThenConstraintGuidance(t *testing.T) {
	report, err := suggest.Advise("ALTER TABLE t ADD COLUMN email text UNIQUE")
	require.NoError(t, err)
	require.Len(t, report.Suggestions, 1)
	s := report.Suggestions[0]
	assert.Empty(t, s.Recommended)
	assert.Equal(t, suggest.GuidanceAddColumnThenConstraint, s.Guidance)
}

// An unnamed CHECK or FOREIGN KEY has no constructible rewrite — the
// VALIDATE step needs the name the server assigns only at creation — so
// the advice is the typed manual path, not an error.
func TestAdviseUnnamedCheckAndForeignKeyGetNamingGuidance(t *testing.T) {
	for _, sql := range []string{
		"ALTER TABLE t ADD CHECK (age > 0)",
		"ALTER TABLE t ADD FOREIGN KEY (o) REFERENCES orders (id)",
	} {
		report, err := suggest.Advise(sql)
		require.NoError(t, err, "statement: %s", sql)
		require.Len(t, report.Suggestions, 1, "statement: %s", sql)
		s := report.Suggestions[0]
		assert.Empty(t, s.Recommended, "statement: %s", sql)
		assert.Equal(t, suggest.GuidanceNameConstraintThenValidate, s.Guidance, "statement: %s", sql)
	}
}

// Following add-column-then-constraint literally must not land on a second
// refusal: the plain column add is clean, and the separate, named ADD
// CONSTRAINT the guidance describes gets a constructed rewrite — not
// guidance again. This chains the two hops for both inline forms whose
// unnamed ADD CONSTRAINT counterpart is itself refused.
func TestAdviseAddColumnGuidanceChainEndsInConstructedRewrite(t *testing.T) {
	for _, tc := range []struct {
		inline string
		named  string
	}{
		{
			inline: "ALTER TABLE t ADD COLUMN x int CHECK (x > 0)",
			named:  "ALTER TABLE t ADD CONSTRAINT t_x_chk CHECK (x > 0)",
		},
		{
			inline: "ALTER TABLE t ADD COLUMN o bigint REFERENCES orders (id)",
			named:  "ALTER TABLE t ADD CONSTRAINT t_o_fk FOREIGN KEY (o) REFERENCES orders (id)",
		},
	} {
		report, err := suggest.Advise(tc.inline)
		require.NoError(t, err, "statement: %s", tc.inline)
		require.Len(t, report.Suggestions, 1, "statement: %s", tc.inline)
		assert.Equal(t, suggest.GuidanceAddColumnThenConstraint, report.Suggestions[0].Guidance,
			"statement: %s", tc.inline)

		plain, err := suggest.Advise("ALTER TABLE t ADD COLUMN x int")
		require.NoError(t, err)
		assert.Empty(t, plain.Suggestions, "the plain column add needs no advice")

		followed, err := suggest.Advise(tc.named)
		require.NoError(t, err, "statement: %s", tc.named)
		require.Len(t, followed.Suggestions, 1, "statement: %s", tc.named)
		s := followed.Suggestions[0]
		assert.NotEmpty(t, s.Recommended,
			"the named constraint the guidance describes gets a constructed rewrite: %s", tc.named)
		assert.Empty(t, s.Guidance,
			"following the guidance must not produce another manual path: %s", tc.named)
	}
}

// ManualGuidance is total over every constraint kind the classifier can
// mark safer-idiom: a parser shape that yields a safer-idiom decision with
// no constructed rewrite maps to typed guidance, never an error that costs
// a consumer its whole report.
func TestManualGuidanceCoversEverySaferIdiomConstraintKind(t *testing.T) {
	want := map[statement.ConstraintKind]suggest.Guidance{
		statement.ConstraintPrimaryKey: suggest.GuidanceUniqueIndexThenConstraint,
		statement.ConstraintUnique:     suggest.GuidanceUniqueIndexThenConstraint,
		statement.ConstraintCheck:      suggest.GuidanceNameConstraintThenValidate,
		statement.ConstraintForeignKey: suggest.GuidanceNameConstraintThenValidate,
		statement.ConstraintNotNull:    suggest.GuidanceNotNullScaffold,
	}
	for kind, g := range want {
		got, err := suggest.ManualGuidance(statement.Op{Kind: statement.OpAddConstraint, Constraint: kind}, false)
		require.NoError(t, err, "constraint kind %d", kind)
		assert.Equal(t, g, got, "constraint kind %d", kind)
	}
}

// lint and suggest agree about the same script: every blocking-idiom
// finding has a suggestion for the same statement — a constructed rewrite
// or typed guidance, never silence. This is the workflow contract: lint
// says what is risky, suggest says what to do about it.
func TestAdviseCoversEveryBlockingIdiomLintFinding(t *testing.T) {
	script := `ALTER TABLE orders ALTER COLUMN paid_at SET NOT NULL, ALTER COLUMN shipped_at SET NOT NULL;
ALTER TABLE orders ATTACH PARTITION orders_2026 FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
CREATE INDEX orders_ref_idx ON orders (reference);`

	lintReport, err := lint.Check(script)
	require.NoError(t, err)
	var flagged []int
	for _, f := range lintReport.Findings {
		if f.Code == lint.CodeBlockingIdiom {
			flagged = append(flagged, f.Statement)
		}
	}
	require.Len(t, flagged, 4, "the script exercises the compound, attach, and index paths")

	report, err := suggest.Advise(script)
	require.NoError(t, err)
	var advised []int
	for _, s := range report.Suggestions {
		advised = append(advised, s.Statement)
		hasRewrite := len(s.Recommended) > 0
		hasGuidance := s.Guidance != ""
		assert.True(t, hasRewrite != hasGuidance,
			"statement %d: exactly one of recommended and guidance is present", s.Statement)
	}
	assert.Equal(t, flagged, advised,
		"one suggestion per blocking-idiom finding, in the same statement order")
}

// Every suggestion is complete: a constructed rewrite carries caveats and
// its execution contract, a non-constructible one carries guidance. The
// statements cover every safer-idiom path the planner produces, so an
// unmapped caveat or guidance entry fails here, in CI, not in a consumer's
// report.
func TestAdviseEverySaferIdiomPathIsMapped(t *testing.T) {
	for _, sql := range []string{
		"CREATE INDEX t_c_idx ON t (c)",
		"DROP INDEX t_c_idx",
		"REINDEX INDEX t_c_idx",
		"ALTER TABLE t DETACH PARTITION p",
		"ALTER TABLE t ALTER COLUMN c SET NOT NULL",
		"ALTER TABLE t ADD PRIMARY KEY (id)",
		"ALTER TABLE t ADD CONSTRAINT t_c_key UNIQUE (c)",
		"ALTER TABLE t ADD CONSTRAINT t_age_pos CHECK (age > 0)",
		"ALTER TABLE t ADD CONSTRAINT t_fk FOREIGN KEY (o) REFERENCES orders (id)",
		"ALTER TABLE t ADD CHECK (age > 0)",
		"ALTER TABLE t ADD FOREIGN KEY (o) REFERENCES orders (id)",
		"ALTER TABLE t ATTACH PARTITION p FOR VALUES FROM (1) TO (10)",
		"ALTER TABLE t ADD COLUMN email text UNIQUE",
		"ALTER TABLE t ALTER COLUMN c SET NOT NULL, ADD COLUMN d int",
	} {
		report, err := suggest.Advise(sql)
		require.NoError(t, err, "statement: %s", sql)
		require.NotEmpty(t, report.Suggestions, "statement: %s", sql)
		for _, s := range report.Suggestions {
			if len(s.Recommended) > 0 {
				assert.NotEmpty(t, s.Caveats, "constructed rewrite without caveats: %s", sql)
				assert.Equal(t, planner.ExecutionAutocommit, s.Execution,
					"constructed rewrite without its execution contract: %s", sql)
				assert.Empty(t, s.Guidance, "rewrite and guidance are exclusive: %s", sql)
			} else {
				assert.NotEmpty(t, s.Guidance, "non-constructible advice without guidance: %s", sql)
				assert.Empty(t, s.Caveats, "caveats describe a recommendation: %s", sql)
				assert.Empty(t, s.Execution, "execution describes a recommendation: %s", sql)
			}
		}
	}
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
		"format_version": 2,
		"suggestions": [
			{
				"statement": 1,
				"line": 1,
				"column": 1,
				"original": "CREATE INDEX t_c_idx ON t (c)",
				"operation": "CREATE INDEX t_c_idx",
				"reason": "safer-idiom",
				"recommended": `+string(recommended)+`,
				"execution": "autocommit-each-step",
				"caveats": ["non-transactional", "invalid-index-on-failure"]
			}
		]
	}`, string(raw))
}

// A guidance suggestion omits the rewrite-only keys entirely — recommended,
// execution, and caveats are absent, not null or empty.
func TestReportJSONGuidanceShape(t *testing.T) {
	report, err := suggest.Advise(
		"ALTER TABLE orders ATTACH PARTITION orders_2026 FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')")
	require.NoError(t, err)
	raw, err := json.Marshal(report)
	require.NoError(t, err)
	require.Len(t, report.Suggestions, 1)
	original, err := json.Marshal(report.Suggestions[0].Original)
	require.NoError(t, err)
	operation, err := json.Marshal(report.Suggestions[0].Operation)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"format_version": 2,
		"suggestions": [
			{
				"statement": 1,
				"line": 1,
				"column": 1,
				"original": `+string(original)+`,
				"operation": `+string(operation)+`,
				"reason": "safer-idiom",
				"guidance": "pre-add-validated-check"
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
		"format_version": 2,
		"suggestions": []
	}`, string(raw))
}
