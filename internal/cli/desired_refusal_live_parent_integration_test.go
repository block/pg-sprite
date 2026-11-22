package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/verdict"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A desired file for an existing child table, PARTITION OF or INHERITS a
// parent, is refused before the scratch build, whether or not it declares
// row security: diff and migrate --desired --dry-run print the refusal
// verdict naming the file's CREATE TABLE and exit 2, and nothing changes.
func TestDesiredFileRefusesALiveParent(t *testing.T) {
	cases := []struct {
		name   string
		setup  string
		create string
	}{
		{
			name: "partition of",
			setup: `CREATE TABLE %[1]s.events (id int NOT NULL, at date NOT NULL) PARTITION BY RANGE (at);
CREATE TABLE %[1]s.documents PARTITION OF %[1]s.events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');`,
			create: "CREATE TABLE documents PARTITION OF events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')",
		},
		{
			name: "inherits",
			setup: `CREATE TABLE %[1]s.events (id int NOT NULL);
CREATE TABLE %[1]s.documents () INHERITS (%[1]s.events);`,
			create: "CREATE TABLE documents () INHERITS (events)",
		},
	}
	files := []struct {
		name   string
		suffix string
	}{
		{name: "plain file", suffix: ";\n"},
		{name: "row-security file", suffix: ";\nALTER TABLE documents ENABLE ROW LEVEL SECURITY;\n"},
	}
	for _, tc := range cases {
		for _, f := range files {
			t.Run(tc.name+"/"+f.name, func(t *testing.T) {
				url := testutil.StartPostgres(t)
				pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
				require.NoError(t, err)
				t.Cleanup(pool.Close)
				schema := testutil.NewSchema(t, pool)
				_, err = pool.Exec(t.Context(), fmt.Sprintf(tc.setup, schema))
				require.NoError(t, err)
				before, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
				require.NoError(t, err)

				file := tc.create + f.suffix
				cmd := desiredCommand(t, url, schema, file, "--dry-run")
				assertRefused := func(t *testing.T, out string, err error) {
					t.Helper()
					require.ErrorIs(t, err, verdict.ErrRefused)
					var v verdict.Verdict
					require.NoError(t, json.Unmarshal([]byte(out), &v), out)
					assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
					assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)
					assert.Equal(t, verdict.ClassCapabilityBoundary, v.Class)
					assert.Equal(t, tc.create, v.Statement)
					assert.True(t, strings.HasSuffix(v.Detail, "nothing was planned or executed"), v.Detail)
				}

				var out strings.Builder
				diff := &DiffCmd{DBFlags: DBFlags{URL: url}, Schema: schema, Desired: cmd.Desired, JSON: true}
				err = diff.run(t.Context(), &out)
				assertRefused(t, out.String(), err)

				out.Reset()
				err = cmd.run(t.Context(), &out)
				assertRefused(t, out.String(), err)

				after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
				require.NoError(t, err)
				assert.Equal(t, before, after, "nothing ran")
			})
		}
	}
}
