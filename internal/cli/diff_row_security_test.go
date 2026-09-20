package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiffUnrelatedInvalidSQLKeepsAdmissionError(t *testing.T) {
	file := filepath.Join(t.TempDir(), "schema.sql")
	sql := `CREATE TABLE documents (id bigint PRIMARY KEY);
 CREATE VIEW visible AS SELECT 1;`
	require.NoError(t, os.WriteFile(file, []byte(sql), 0600))
	_, want := statement.ParseDesired(sql)
	cmd := &DiffCmd{Desired: file}
	var out strings.Builder
	err := cmd.run(t.Context(), &out)
	require.EqualError(t, err, want.Error())
	require.ErrorIs(t, err, statement.ErrDisallowedStatement)
	require.NotErrorIs(t, err, verdict.ErrRefused)
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
