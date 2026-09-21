package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRefusedStatementSQLIsOnlyComments(t *testing.T) {
	for _, ending := range []string{"\r", "\n", "\r\n"} {
		t.Run(fmt.Sprintf("%q", ending), func(t *testing.T) {
			var out strings.Builder
			require.NoError(t, writeChangeText(&out, plan.Statement{
				SQL: `CREATE TABLE "documents` + ending + `SELECT 42; --" (id bigint)`, Disposition: router.DispositionRefuse,
			}))
			_, err := statement.ParseDesired(out.String())
			require.ErrorIs(t, err, statement.ErrEmptyDesired)
		})
	}
}

func TestMissingTableHeaderIsOnlyComments(t *testing.T) {
	absent := false
	for _, ending := range []string{"\r", "\n", "\r\n"} {
		t.Run(fmt.Sprintf("%q", ending), func(t *testing.T) {
			var out strings.Builder
			report := plan.Report{Schema: "public" + ending + "SELECT 42; --", Table: "documents" + ending + "SELECT 42; --", TableExists: &absent,
				Statements: []plan.Statement{{SQL: "CREATE TABLE documents (id bigint)", Disposition: router.DispositionRefuse}}}
			require.NoError(t, writePlanText(&out, report))
			_, err := statement.ParseDesired(out.String())
			require.ErrorIs(t, err, statement.ErrEmptyDesired)
		})
	}
}

func TestSaferSQLAnnotationDoesNotAddStatements(t *testing.T) {
	for _, ending := range []string{"\r", "\n", "\r\n"} {
		t.Run(fmt.Sprintf("%q", ending), func(t *testing.T) {
			var out strings.Builder
			require.NoError(t, writeChangeText(&out, plan.Statement{
				SQL: "CREATE TABLE documents (id bigint)", Disposition: router.DispositionExecute,
				ExecSQL: []string{"annotation" + ending + "SELECT 42; --"},
			}))
			parsed, err := statement.ParseDesired(out.String())
			require.NoError(t, err)
			assert.Len(t, parsed.Statements(), 1, "only the intended table statement may remain")
		})
	}
}
