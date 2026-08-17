package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/verdict"
)

// The statement-type gate applies to a dry run exactly as it does to an
// apply: a gated kind must not dry-run to an executable plan and then
// refuse on apply. Offline — the gate decides before any connection is
// made, so no database is needed.
func TestDryRunGatesUnsupportedStatementKinds(t *testing.T) {
	cases := []struct {
		name   string
		alter  string
		reason verdict.Reason
	}{
		{"drop index", "DROP INDEX users_email_idx", verdict.ReasonIndexStatement},
		{"reindex", "REINDEX TABLE users", verdict.ReasonIndexStatement},
		{"create table", "CREATE TABLE t (id int)", verdict.ReasonUnsupportedStatement},
		{"other statement kind", "TRUNCATE users", verdict.ReasonUnsupportedStatement},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &MigrateCmd{Alter: tc.alter, DryRun: true, JSON: true}
			var out strings.Builder
			err := c.run(t.Context(), &out)
			require.ErrorIs(t, err, verdict.ErrRefused,
				"a gated kind must exit with the refusal code in a dry run too")

			var v verdict.Verdict
			require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
			assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
			assert.Equal(t, tc.reason, v.Reason)
			assert.Equal(t, tc.alter, v.Statement)
		})
	}
}
