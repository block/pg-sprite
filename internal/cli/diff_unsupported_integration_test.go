package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/verdict"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A desired file that differs from its live table in a way the diff does not
// converge is refused with its class and owner and exit 2 by diff,
// migrate --desired --dry-run, and migrate --desired, and nothing runs. A
// tuned storage parameter is the owner's to change directly; a persistence
// or collation change needs a rewrite the planner does not construct; a
// serial column to add is a shape the plan cannot carry.
func TestDesiredFileRefusesAChangeTheDiffDoesNotDerive(t *testing.T) {
	cases := []struct {
		name  string
		setup string
		file  string
		class verdict.Class
		owner verdict.Owner
	}{
		{
			name:  "storage parameters",
			setup: `ALTER TABLE %s.documents SET (fillfactor = 70)`,
			file:  "CREATE TABLE documents (\n id int PRIMARY KEY,\n owner_id int NOT NULL\n);",
			class: verdict.ClassNoOnlineSafetyProblem,
			owner: verdict.OwnerDirectOperator,
		},
		{
			name:  "persistence",
			setup: `ALTER TABLE %s.documents SET UNLOGGED`,
			file:  "CREATE TABLE documents (\n id int PRIMARY KEY,\n owner_id int NOT NULL\n);",
			class: verdict.ClassCapabilityBoundary,
		},
		{
			name:  "collation change",
			setup: `ALTER TABLE %s.documents ADD COLUMN title text`,
			file:  "CREATE TABLE documents (\n id int PRIMARY KEY,\n owner_id int NOT NULL,\n title text COLLATE \"C\"\n);",
			class: verdict.ClassCapabilityBoundary,
		},
		{
			name:  "serial column",
			file:  "CREATE TABLE documents (\n id int PRIMARY KEY,\n owner_id int NOT NULL,\n seq serial\n);",
			class: verdict.ClassCapabilityBoundary,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url, schema, pool := desiredFixture(t)
			if tc.setup != "" {
				_, err := pool.Exec(t.Context(), fmt.Sprintf(tc.setup, schema))
				require.NoError(t, err)
			}
			before, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
			require.NoError(t, err)
			assertRefused := func(t *testing.T, out string, err error) {
				t.Helper()
				require.ErrorIs(t, err, verdict.ErrRefused)
				var v verdict.Verdict
				require.NoError(t, json.Unmarshal([]byte(out), &v), out)
				assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
				assert.Equal(t, tc.class, v.Class)
				assert.Equal(t, tc.owner, v.Owner)
				assert.Contains(t, v.Detail, schemadiff.ErrUnsupportedChange.Error())
				assert.True(t, strings.HasSuffix(v.Detail, "nothing was planned or executed"), v.Detail)
			}

			cmd := desiredCommand(t, url, schema, tc.file)
			var out strings.Builder
			diff := &DiffCmd{DBFlags: DBFlags{URL: url}, Schema: schema, Desired: cmd.Desired, JSON: true}
			err = diff.run(t.Context(), &out)
			assertRefused(t, out.String(), err)

			cmd.DryRun = true
			out.Reset()
			err = cmd.run(t.Context(), &out)
			assertRefused(t, out.String(), err)

			cmd.DryRun = false
			out.Reset()
			err = cmd.run(t.Context(), &out)
			assertRefused(t, out.String(), err)

			after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
			require.NoError(t, err)
			assert.Equal(t, before, after, "nothing ran")
		})
	}
}
