package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/block/pg-sprite/pkg/statement"

	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/verdict"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSecurityReviewSQLContainsOnlyComments(t *testing.T) {
	before, after := true, false
	review := &diffplan.RowSecurityReviewRequired{Schema: "public", Table: "documents\nodd_name", Review: schemadiff.RowSecurityReview{
		Version: 1, TableComparisonComplete: true, TableChanged: true,
		Changes: []schemadiff.SecurityChange{{Kind: schemadiff.SecurityEnabled, BeforeSetting: &before, AfterSetting: &after, Impact: schemadiff.AccessMayWiden}},
	}}
	cmd := &DiffCmd{SQL: true}
	var out strings.Builder
	require.ErrorIs(t, cmd.writeRowSecurityRefusal(&out, review, "CREATE TABLE documents (id bigint)"), verdict.ErrRefused)
	for line := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		assert.True(t, strings.HasPrefix(line, "--"), line)
	}
	assert.Contains(t, out.String(), "true → false")
	assert.Contains(t, out.String(), "Table changes also present")
}

func TestSecurityReviewSQLCommentsContainAllLineEndings(t *testing.T) {
	for _, separator := range []string{"\r", "\n", "\r\n"} {
		t.Run(fmt.Sprintf("%q", separator), func(t *testing.T) {
			payload := "documents" + separator + "SELECT 42; --"
			review := &diffplan.RowSecurityReviewRequired{Schema: payload, Table: payload, Review: schemadiff.RowSecurityReview{
				Version: 1, TableComparisonError: payload,
			}}
			cmd := &DiffCmd{SQL: true}
			var out strings.Builder
			require.ErrorIs(t, cmd.writeRowSecurityRefusal(&out, review, "CREATE TABLE documents (id bigint)"), verdict.ErrRefused)
			// ParseDesired returns ErrEmptyDesired only when PostgreSQL's parser
			// produced zero statements, not when statements merely fail admission.
			_, err := statement.ParseDesired(out.String())
			require.ErrorIs(t, err, statement.ErrEmptyDesired, out.String())
			out.Reset()
			require.ErrorIs(t, cmd.writeRowSecurityRefusal(&out, errors.New(payload), "CREATE TABLE documents (id bigint)"), verdict.ErrRefused)
			_, err = statement.ParseDesired(out.String())
			require.ErrorIs(t, err, statement.ErrEmptyDesired, out.String())
		})
	}
}

func TestSecurityReviewDefaultTextIncludesReview(t *testing.T) {
	before, after := true, false
	review := &diffplan.RowSecurityReviewRequired{Schema: "public", Table: "documents", Review: schemadiff.RowSecurityReview{
		Version: 1, TableComparisonComplete: true,
		Changes: []schemadiff.SecurityChange{{Kind: schemadiff.SecurityEnabled, BeforeSetting: &before, AfterSetting: &after, Impact: schemadiff.AccessMayWiden}},
	}}
	var out strings.Builder
	cmd := &DiffCmd{}
	require.ErrorIs(t, cmd.writeRowSecurityRefusal(&out, review, "CREATE TABLE documents (id bigint)"), verdict.ErrRefused)
	assert.Contains(t, out.String(), "public.documents — row security review")
	assert.Contains(t, out.String(), "enabled: true → false [may-widen]")
	assert.Contains(t, out.String(), "migrate --desired works out the change again under its own lock")
}

func TestSecurityReviewJSONIsIndented(t *testing.T) {
	review := &diffplan.RowSecurityReviewRequired{Schema: "public", Table: "documents", Review: schemadiff.RowSecurityReview{
		Version: 1, Changes: []schemadiff.SecurityChange{}, TableComparisonComplete: true,
	}}
	var out strings.Builder
	cmd := &DiffCmd{JSON: true}
	require.ErrorIs(t, cmd.writeRowSecurityRefusal(&out, review, "CREATE TABLE documents (id bigint)"), verdict.ErrRefused)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(out.String()), &decoded))
	expected, err := json.MarshalIndent(json.RawMessage(out.String()), "", "  ")
	require.NoError(t, err)
	assert.Equal(t, string(expected)+"\n", out.String())
	assert.Equal(t, "refused", decoded["outcome"])
}

// A refused row security change names the file's CREATE TABLE and says how
// to apply the file: migrate --desired for the row security on its own, two
// runs when the file also changes the table, and neither for a table shape
// migrate --desired refuses. The detail reads the same in text, JSON, and
// SQL output, so it never points at output printed beside it.
func TestRowSecurityRefusalSaysHowToApplyTheFile(t *testing.T) {
	before, after := true, false
	setting := []schemadiff.SecurityChange{{Kind: schemadiff.SecurityEnabled, BeforeSetting: &before, AfterSetting: &after, Impact: schemadiff.AccessMayWiden}}
	const create = "CREATE TABLE documents (id bigint)"
	for name, tc := range map[string]struct {
		review schemadiff.RowSecurityReview
		shape  error
		detail string
	}{
		"a table shape migrate --desired does not change": {
			review: schemadiff.RowSecurityReview{Version: 1, TableComparisonComplete: true, Changes: setting},
			shape:  executor.RowSecurityTableShapeRefusal(schemadiff.Model{Table: "documents", PartitionKey: "RANGE (id)"}),
			detail: "migrate --desired does not change row security on this table: " +
				"row security target: partitioned parent (PARTITION BY RANGE (id)): " + schemadiff.ErrUnrenderablePartitionedRowSecurity.Error(),
		},
		"row security alone": {
			review: schemadiff.RowSecurityReview{Version: 1, TableComparisonComplete: true, Changes: setting},
			detail: "row security is not planned ahead: migrate --desired works out the change again under its own lock " +
				"and applies what differs then in one transaction, so review this output right before you run it",
		},
		"row security and the table": {
			review: schemadiff.RowSecurityReview{Version: 1, TableComparisonComplete: true, TableChanged: true, Changes: setting},
			detail: "the file changes both the table and its row security, which are applied in separate runs: " +
				"first run migrate --desired with the table definition as it is live and the new row security, " +
				"then change the table definition in a second run",
		},
		"a table that does not compare": {
			review: schemadiff.RowSecurityReview{Version: 1, TableComparisonError: "unsupported schema change", Changes: setting},
			detail: "the table definition does not compare with the live table, so apply the row security on its own: " +
				"run migrate --desired with the table definition as it is live and the new row security, " +
				"then plan the table change separately",
		},
	} {
		t.Run(name, func(t *testing.T) {
			review := &diffplan.RowSecurityReviewRequired{Schema: "public", Table: "documents", Review: tc.review, ShapeRefusal: tc.shape}

			var text strings.Builder
			require.ErrorIs(t, (&DiffCmd{}).writeRowSecurityRefusal(&text, review, create), verdict.ErrRefused)
			assert.Contains(t, text.String(), "statement: "+create+"\n")
			assert.Contains(t, text.String(), "detail:    "+tc.detail+"\n")
			assert.NotContains(t, text.String(), "atomic RLS executor")

			var asJSON strings.Builder
			require.ErrorIs(t, (&DiffCmd{JSON: true}).writeRowSecurityRefusal(&asJSON, review, create), verdict.ErrRefused)
			var v verdict.Verdict
			require.NoError(t, json.Unmarshal([]byte(asJSON.String()), &v))
			assert.Equal(t, create, v.Statement)
			assert.Equal(t, tc.detail, v.Detail)

			var asSQL strings.Builder
			require.ErrorIs(t, (&DiffCmd{SQL: true}).writeRowSecurityRefusal(&asSQL, review, create), verdict.ErrRefused)
			assert.Contains(t, asSQL.String(), "-- refused: "+tc.detail)
		})
	}
}
