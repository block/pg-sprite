package cli

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/block/pg-sprite/pkg/statement"

	"github.com/block/pg-sprite/pkg/diffplan"
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
	require.ErrorIs(t, cmd.writeRowSecurityRefusal(&out, review), verdict.ErrRefused)
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
			require.ErrorIs(t, cmd.writeRowSecurityRefusal(&out, review), verdict.ErrRefused)
			// ParseDesired returns ErrEmptyDesired only when PostgreSQL's parser
			// produced zero statements, not when statements merely fail admission.
			_, err := statement.ParseDesired(out.String())
			require.ErrorIs(t, err, statement.ErrEmptyDesired, out.String())
			out.Reset()
			require.ErrorIs(t, cmd.writeRowSecurityRefusal(&out, errors.New(payload)), verdict.ErrRefused)
			_, err = statement.ParseDesired(out.String())
			require.ErrorIs(t, err, statement.ErrEmptyDesired, out.String())
		})
	}
}
