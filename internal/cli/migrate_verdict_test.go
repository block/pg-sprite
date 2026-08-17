package cli

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/suggest"
	"github.com/block/pg-sprite/pkg/verdict"
)

// routeOne runs one statement through the same offline pipeline the run
// path uses — parse, canonicalize, classify with no live facts, route —
// so these tests exercise the production classification, not fixtures.
func routeOne(t *testing.T, sql string) (statement.Statement, router.Statement) {
	t.Helper()
	st, err := statement.ParseOne(sql)
	require.NoError(t, err)
	canonical, err := statement.Canonical(st.SQL())
	require.NoError(t, err)
	classified, err := planner.Classify(canonical, planner.Facts{})
	require.NoError(t, err)
	routed := router.Route([]planner.Plan{classified})
	require.Len(t, routed.Statements, 1)
	return st, routed.Statements[0]
}

// Every rewrite-required shape the planner produces ends in a refusal
// verdict carrying its typed guidance — never in an operational error. The
// unnamed constraint forms are the regression case: their guidance comes
// from the suggest vocabulary's naming path.
func TestRewriteRequiredVerdictCoversEveryShape(t *testing.T) {
	for _, tc := range []struct {
		sql      string
		guidance suggest.Guidance
	}{
		{"ALTER TABLE users ADD CHECK (age > 0)", suggest.GuidanceNameConstraintThenValidate},
		{"ALTER TABLE users ADD FOREIGN KEY (org_id) REFERENCES orgs (id)", suggest.GuidanceNameConstraintThenValidate},
		{"ALTER TABLE users ADD COLUMN nickname text UNIQUE", suggest.GuidanceAddColumnThenConstraint},
		{"ALTER TABLE orders ATTACH PARTITION orders_2026 FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')", suggest.GuidancePrevalidatedCheck},
		{"ALTER TABLE users ADD COLUMN nickname text UNIQUE, ADD COLUMN age int", suggest.GuidanceSplitStatement},
	} {
		st, rs := routeOne(t, tc.sql)
		require.Equal(t, router.DispositionRewriteRequired, rs.Disposition,
			"statement must route rewrite-required: %s", tc.sql)

		v, err := rewriteRequiredVerdict(st, rs)
		require.NoError(t, err, "statement: %s", tc.sql)
		assert.Equal(t, verdict.OutcomeRefused, v.Outcome, "statement: %s", tc.sql)
		assert.Equal(t, verdict.ReasonRewriteRequired, v.Reason, "statement: %s", tc.sql)
		assert.Equal(t, string(tc.guidance), v.Guidance, "statement: %s", tc.sql)
	}
}

// A rewrite-required refusal keeps the exit-code contract: emit prints the
// verdict and returns ErrRefused, in both output formats.
func TestRewriteRequiredVerdictEmitsRefusalExit(t *testing.T) {
	st, rs := routeOne(t, "ALTER TABLE users ADD CHECK (age > 0)")
	v, err := rewriteRequiredVerdict(st, rs)
	require.NoError(t, err)

	for _, jsonMode := range []bool{false, true} {
		c := &MigrateCmd{JSON: jsonMode}
		var out bytes.Buffer
		assert.ErrorIs(t, c.emit(&out, v), verdict.ErrRefused, "json=%v", jsonMode)
		assert.NotEmpty(t, out.String(), "json=%v", jsonMode)
	}
}

// The text form expands the guidance token into its manual path and links
// the vocabulary, so a human is not left to look the code up; the JSON
// form stays tokens-only. Renderer unit test: wording is the surface here.
func TestEmitTextExpandsGuidance(t *testing.T) {
	st, rs := routeOne(t, "ALTER TABLE users ADD CHECK (age > 0)")
	v, err := rewriteRequiredVerdict(st, rs)
	require.NoError(t, err)

	var text bytes.Buffer
	assert.ErrorIs(t, (&MigrateCmd{}).emit(&text, v), verdict.ErrRefused)
	assert.Contains(t, text.String(), "help:      "+guidanceText(suggest.GuidanceNameConstraintThenValidate))
	assert.Contains(t, text.String(), suggestReportURL+"#"+string(suggest.GuidanceNameConstraintThenValidate),
		"the reference links the guidance code's own doc anchor")

	var raw bytes.Buffer
	assert.ErrorIs(t, (&MigrateCmd{JSON: true}).emit(&raw, v), verdict.ErrRefused)
	assert.NotContains(t, raw.String(), "help:", "the JSON form carries the typed token only")
}
