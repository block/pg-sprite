package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/verdict"
)

// The desired file for the partitioned events table below, without its
// legacy column.
const eventsWithoutLegacy = `CREATE TABLE events (
 id bigint NOT NULL,
 created_at timestamptz NOT NULL,
 region text NOT NULL,
 note text
 ) PARTITION BY RANGE (created_at);`

// An operator reviews a desired file that removes a column of a
// partitioned table, one of whose partitions is list-partitioned again.
// The diff plans one native drop; the run with the pin and the
// acknowledgement drops the column from the parent and every partition
// and reports executed, as for an ordinary table. When a partition carries
// an index of its own on the column, the same run fails with the typed
// code and drops nothing.
func TestMigrateDesiredDropsAColumnOfAPartitionedTable(t *testing.T) {
	for _, tc := range []struct {
		name      string
		partition string
		code      string
		legacyOn  int
	}{
		{name: "every partition loses the column", legacyOn: 0},
		{
			name:      "a partition's own index refuses the drop",
			partition: `CREATE INDEX events_2025_legacy_idx ON %s.events_2025 (legacy)`,
			code:      string(executor.CodeDroppedColumnPartitionDependent),
			legacyOn:  6,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := testutil.StartPostgres(t)
			pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
			require.NoError(t, err)
			t.Cleanup(pool.Close)
			schema := testutil.NewSchema(t, pool)
			_, err = pool.Exec(t.Context(), fmt.Sprintf(`
				CREATE TABLE %[1]s.events (
					id bigint NOT NULL,
					created_at timestamptz NOT NULL,
					region text NOT NULL,
					note text,
					legacy text
				) PARTITION BY RANGE (created_at);
				CREATE TABLE %[1]s.events_2024 PARTITION OF %[1]s.events
					FOR VALUES FROM ('2024-01-01') TO ('2025-01-01');
				CREATE TABLE %[1]s.events_2025 PARTITION OF %[1]s.events
					FOR VALUES FROM ('2025-01-01') TO ('2026-01-01');
				CREATE TABLE %[1]s.events_2026 PARTITION OF %[1]s.events
					FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')
					PARTITION BY LIST (region);
				CREATE TABLE %[1]s.events_2026_eu PARTITION OF %[1]s.events_2026 FOR VALUES IN ('eu');
				CREATE TABLE %[1]s.events_2026_us PARTITION OF %[1]s.events_2026 FOR VALUES IN ('us');
				INSERT INTO %[1]s.events VALUES
					(1, '2024-05-01', 'eu', 'a', 'x'),
					(2, '2025-05-01', 'us', 'b', 'y'),
					(3, '2026-05-01', 'eu', 'c', 'z')`, schema))
			require.NoError(t, err)
			if tc.partition != "" {
				_, err = pool.Exec(t.Context(), fmt.Sprintf(tc.partition, schema))
				require.NoError(t, err)
			}

			cmd := desiredCommand(t, url, schema, eventsWithoutLegacy)
			diff := &DiffCmd{DBFlags: DBFlags{URL: url}, Schema: schema, Desired: cmd.Desired, JSON: true}
			var out strings.Builder
			require.NoError(t, diff.run(t.Context(), &out))
			var reviewed plan.Report
			require.NoError(t, json.Unmarshal([]byte(out.String()), &reviewed))
			require.Len(t, reviewed.Statements, 1)
			require.True(t, reviewed.Statements[0].Destructive)

			run := desiredCommand(t, url, schema, eventsWithoutLegacy,
				"--expect-fingerprint", reviewed.Fingerprint, "--allow-destructive", schema+".events")
			out.Reset()
			runErr := run.run(t.Context(), &out)
			var result migrate.DesiredResult
			require.NoError(t, json.Unmarshal([]byte(out.String()), &result))
			require.Len(t, result.Verdicts, 1)
			assert.True(t, result.Verdicts[0].AllowedDestructive)
			if tc.code == "" {
				require.NoError(t, runErr)
				assert.Equal(t, verdict.OutcomeExecuted, result.Outcome, result.Detail)
			} else {
				require.Error(t, runErr)
				assert.Equal(t, verdict.OutcomeFailed, result.Outcome)
				assert.Equal(t, tc.code, result.Verdicts[0].Code)
			}

			var legacyOn int
			require.NoError(t, pool.QueryRow(t.Context(), `
				SELECT count(*)
				FROM pg_attribute a
				JOIN pg_class c ON c.oid = a.attrelid
				WHERE c.relnamespace = $1::regnamespace
					AND c.relname LIKE 'events%'
					AND c.relkind IN ('r', 'p')
					AND a.attname = 'legacy'
					AND NOT a.attisdropped`, schema).Scan(&legacyOn))
			assert.Equal(t, tc.legacyOn, legacyOn, "the tables of the tree that keep the column")
		})
	}
}
