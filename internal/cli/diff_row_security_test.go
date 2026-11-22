package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/block/pg-sprite/pkg/verdict"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A file with no row security that holds a statement the desired file may
// not hold is refused as a desired-file statement, classed by its kind, not
// as a row-security review.
func TestDiffRefusesAStatementTheDesiredFileCannotHold(t *testing.T) {
	file := filepath.Join(t.TempDir(), "schema.sql")
	sql := `CREATE TABLE documents (id bigint PRIMARY KEY);
 CREATE VIEW visible AS SELECT 1;`
	require.NoError(t, os.WriteFile(file, []byte(sql), 0600))
	cmd := &DiffCmd{Desired: file, JSON: true}
	var out strings.Builder
	err := cmd.run(t.Context(), &out)
	require.ErrorIs(t, err, verdict.ErrRefused)
	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.ClassNoOnlineSafetyProblem, v.Class)
	assert.Equal(t, verdict.OwnerDirectOperator, v.Owner)
	assert.Equal(t, "CREATE VIEW visible AS SELECT 1", v.Statement)
}

func TestRowSecurityRefusalOutput(t *testing.T) {
	for _, format := range []string{"text", "json", "sql"} {
		t.Run(format, func(t *testing.T) {
			cmd := &DiffCmd{JSON: format == "json", SQL: format == "sql"}
			var out strings.Builder
			err := cmd.writeRowSecurityRefusal(&out, errors.New("row security differs"))
			require.ErrorIs(t, err, verdict.ErrRefused)
			if format == "json" {
				var v verdict.Verdict
				require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
				assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
				assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)
				assert.Equal(t, "row security differs", v.Detail)
			} else {
				assert.Contains(t, out.String(), "row security differs")
				if format == "sql" {
					assert.True(t, strings.HasPrefix(out.String(), "-- refused:"))
				}
			}
		})
	}
}
