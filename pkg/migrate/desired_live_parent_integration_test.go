package migrate_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/verdict"
)

// A desired file for an existing partition or inheritance child names its
// live parent. Planning it would lock that parent, so RunDesired returns a
// typed refusal an operator can act on, rather than an operational error,
// and runs nothing.
func TestRunDesiredRefusesAFileThatNamesALiveParent(t *testing.T) {
	url := testutil.NewDatabase(t, testutil.StartPostgres(t))
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	cases := []struct {
		name, live, desired, detail string
	}{
		{
			name: "partition of",
			live: `CREATE TABLE %[1]s.metrics (id bigint NOT NULL, day date NOT NULL) PARTITION BY RANGE (day);
				CREATE TABLE %[1]s.metrics_p1 PARTITION OF %[1]s.metrics FOR VALUES FROM ('2026-01-01') TO ('2026-02-01')`,
			desired: "CREATE TABLE metrics_p1 PARTITION OF metrics FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');",
			detail:  "PARTITION OF cannot be planned",
		},
		{
			name: "inherits",
			live: `CREATE TABLE %[1]s.parent (id bigint NOT NULL);
				CREATE TABLE %[1]s.child (note text) INHERITS (%[1]s.parent)`,
			desired: "CREATE TABLE child (note text) INHERITS (parent);",
			detail:  "INHERITS cannot be planned",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			schema := testutil.NewSchema(t, pool)
			_, err := pool.Exec(t.Context(), fmt.Sprintf(tc.live, schema))
			require.NoError(t, err)

			res, err := migrate.RunDesired(t.Context(), pool,
				migrate.DesiredRequest{Schema: schema, Desired: parseDesired(t, tc.desired)}, runOptions())
			require.NoError(t, err, "a file naming a live parent is a refusal, not an error")
			assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
			assert.Equal(t, verdict.ReasonUnsupportedStatement, res.Reason)
			assert.Contains(t, res.Detail, tc.detail)
			assert.Contains(t, res.Detail, "nothing was planned or executed")
			assert.Empty(t, res.Verdicts, "nothing was attempted")
		})
	}
}
