package planner_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/planner"
)

// The native backend runs the online sequence in place of an ADD PRIMARY
// KEY / UNIQUE, so the sequence has to leave the table exactly as the
// statement would have. Each case runs the statement as written on one
// table and the planner's sequence on an identical table in another schema,
// then compares the two through the server's catalogs: a sequence that
// drops NULLS NOT DISTINCT accepts a second NULL key, one that drops
// DEFERRABLE checks every row at the statement instead of at commit, and
// one that drops INCLUDE builds a different index.
//
// CREATE INDEX CONCURRENTLY waits out every older transaction in its
// database, so the test runs in a database of its own rather than beside
// other packages' open transactions.
func TestUsingIndexSequenceDeclaresTheSameConstraint(t *testing.T) {
	url := testutil.NewDatabase(t, testutil.StartPostgres(t))
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	var version int
	require.NoError(t, pool.QueryRow(t.Context(),
		"SELECT current_setting('server_version_num')::int").Scan(&version))

	cases := []struct {
		name string
		key  string
		// minVersion is the first server_version_num whose grammar accepts
		// the key; zero for every supported version.
		minVersion int
	}{
		{"plain unique", "UNIQUE (email)", 0},
		{"plain primary key", "PRIMARY KEY (id)", 0},
		{"include columns", "UNIQUE (email) INCLUDE (name, id)", 0},
		{"nulls not distinct", "UNIQUE NULLS NOT DISTINCT (email)", 150000},
		{"deferrable", "UNIQUE (email) DEFERRABLE", 0},
		{"deferrable initially deferred", "UNIQUE (email) DEFERRABLE INITIALLY DEFERRED", 0},
		{"deferrable primary key", "PRIMARY KEY (id) DEFERRABLE INITIALLY DEFERRED", 0},
		{"every attribute at once", "UNIQUE NULLS NOT DISTINCT (email, name) INCLUDE (id) DEFERRABLE INITIALLY DEFERRED", 150000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if version < tc.minVersion {
				t.Skipf("server %d predates the grammar for %s", version, tc.key)
			}
			direct := testutil.NewSchema(t, pool)
			sequenced := testutil.NewSchema(t, pool)
			for _, schema := range []string{direct, sequenced} {
				_, err := pool.Exec(t.Context(), fmt.Sprintf(
					"CREATE TABLE %s.t (id int NOT NULL, email text NOT NULL, name text)", schema))
				require.NoError(t, err)
			}

			_, err := pool.Exec(t.Context(), fmt.Sprintf("ALTER TABLE %s.t ADD CONSTRAINT k %s", direct, tc.key))
			require.NoError(t, err)

			plan, err := planner.Classify(fmt.Sprintf("ALTER TABLE %s.t ADD CONSTRAINT k %s", sequenced, tc.key), planner.Facts{})
			require.NoError(t, err)
			require.Len(t, plan.Decisions, 1)
			require.Len(t, plan.Decisions[0].SaferSQL, 2, "the planner builds the online sequence for %s", tc.key)
			for _, step := range plan.Decisions[0].SaferSQL {
				_, err := pool.Exec(t.Context(), step)
				require.NoError(t, err, step)
			}

			want, err := testutil.ReadTableCatalog(t.Context(), pool, direct, "t")
			require.NoError(t, err)
			got, err := testutil.ReadTableCatalog(t.Context(), pool, sequenced, "t")
			require.NoError(t, err)
			assert.Equal(t, want.Constraints, got.Constraints, "constraint after the sequence for %s", tc.key)
			assert.Equal(t, want.Indexes, got.Indexes, "index after the sequence for %s", tc.key)
		})
	}
}
