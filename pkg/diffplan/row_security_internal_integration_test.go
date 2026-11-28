package diffplan

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// The table definition of a row security file is planned only after its row
// security matched the live table. A table dropped between the two is
// reported as missing, never planned as a create of the table without its
// row security.
func TestPlanTableDefinitionReportsATableDroppedAfterItsRowSecurityMatched(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	desired, err := statement.ParseDesiredWithRowSecurity(`CREATE TABLE t (id int PRIMARY KEY);
ALTER TABLE t ENABLE ROW LEVEL SECURITY;`)
	require.NoError(t, err)

	report, err := planTableDefinition(t.Context(), pool, schema, desired)
	require.ErrorIs(t, err, schemadiff.ErrTableNotFound)
	assert.ErrorContains(t, err, schema+".t was dropped while its table definition was planned")
	assert.Empty(t, report.Statements)
}

// The comparison finds only the table differing, and before its definition
// is planned another session adds the file's column. When that session also
// changes the policy, neither read saw the whole file matching, so the plan
// is the row security difference, never an empty plan. When it changes only
// the table, the file matches and the plan is empty.
func TestPlanWithRowSecurityComparesTheWholeFileWhenTheTableChangesWhileItIsPlanned(t *testing.T) {
	for _, tc := range []struct {
		name       string
		concurrent string
		differs    bool
	}{
		{
			name: "the policy changes too",
			concurrent: `ALTER TABLE %[1]s.t ADD COLUMN title text;
DROP POLICY readers ON %[1]s.t;
CREATE POLICY readers ON %[1]s.t FOR SELECT USING (owner_id = 8);`,
			differs: true,
		},
		{
			name:       "only the table changes",
			concurrent: `ALTER TABLE %[1]s.t ADD COLUMN title text;`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, schema := rowSecurityTable(t)
			desired, err := statement.ParseDesiredWithRowSecurity(`CREATE TABLE t (id int PRIMARY KEY, owner_id int NOT NULL, title text);
ALTER TABLE t ENABLE ROW LEVEL SECURITY;
CREATE POLICY readers ON t FOR SELECT USING (owner_id = 7);`)
			require.NoError(t, err)
			concurrently := func(ctx context.Context) error {
				_, err := pool.Exec(ctx, fmt.Sprintf(tc.concurrent, schema))
				return err
			}

			report, err := planWithRowSecurity(t.Context(), pool, schema, desired, concurrently)
			if !tc.differs {
				require.NoError(t, err)
				assert.Empty(t, report.Statements)
				return
			}
			var review *RowSecurityReviewRequired
			require.True(t, errors.As(err, &review), "the row security differs: %v", err)
			assert.ErrorContains(t, err, schema+".t changed while it was planned; plan it again")
			assert.ErrorIs(t, err, schemadiff.ErrUnsupportedChange)
			assert.Empty(t, report.Statements)
		})
	}
}

// rowSecurityTable returns a pool and a schema holding t with row security
// enabled and one policy.
func rowSecurityTable(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %[1]s.t (id int PRIMARY KEY, owner_id int NOT NULL);
ALTER TABLE %[1]s.t ENABLE ROW LEVEL SECURITY;
CREATE POLICY readers ON %[1]s.t FOR SELECT USING (owner_id = 7);`, schema))
	require.NoError(t, err)
	return pool, schema
}

// A file that retypes a column its policy reads and keeps the policy's text
// does not change the row security, though PostgreSQL prints the policy on
// the new type with a cast the live one lacks. The plan is the table change
// on its own, which refuses the retype for the policy that reads the column. A file that also changes
// the policy, or retypes the column to a type the policy's expression does
// not accept, changes the row security.
func TestPlanWithRowSecurityKeepsAPolicyWhoseTextTheFileKeepsAcrossARetype(t *testing.T) {
	for _, tc := range []struct {
		name, column, using string
		rowSecurityDiffers  bool
		reason              string
	}{
		{name: "the policy text is kept", column: "varchar(100)", using: "owner_name = current_user"},
		{name: "the policy changes too", column: "varchar(100)", using: "owner_name = session_user", rowSecurityDiffers: true},
		{name: "the live policy does not accept the new type", column: "integer", using: "owner_name = 7", rowSecurityDiffers: true,
			reason: "could not be created on the file's table definition"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
			require.NoError(t, err)
			t.Cleanup(pool.Close)
			schema := testutil.NewSchema(t, pool)
			_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %[1]s.t (id int PRIMARY KEY, owner_name text NOT NULL);
ALTER TABLE %[1]s.t ENABLE ROW LEVEL SECURITY;
CREATE POLICY owners ON %[1]s.t USING (owner_name = current_user);`, schema))
			require.NoError(t, err)
			desired, err := statement.ParseDesiredWithRowSecurity(fmt.Sprintf(`CREATE TABLE t (id int PRIMARY KEY, owner_name %s NOT NULL);
ALTER TABLE t ENABLE ROW LEVEL SECURITY;
CREATE POLICY owners ON t USING (%s);`, tc.column, tc.using))
			require.NoError(t, err)

			report, err := PlanWithRowSecurity(t.Context(), pool, schema, desired)
			if tc.rowSecurityDiffers {
				var review *RowSecurityReviewRequired
				require.True(t, errors.As(err, &review), "the row security differs: %v", err)
				if tc.reason != "" {
					assert.ErrorContains(t, err, tc.reason)
				}
				return
			}
			require.NoError(t, err)
			require.Len(t, report.Statements, 1)
			retype := report.Statements[0]
			assert.Equal(t, fmt.Sprintf("ALTER TABLE %s.t ALTER COLUMN owner_name TYPE varchar(100)", schema), retype.SQL)
			assert.Equal(t, router.DispositionRefuse, retype.Disposition)
			require.Len(t, retype.Decisions, 1)
			assert.Equal(t, planner.ReasonDependentPolicy, retype.Decisions[0].Reason)
			assert.Equal(t, []string{fmt.Sprintf(`"owners" ON "%s"."t"`, schema)}, retype.Decisions[0].DependentPolicies)
		})
	}
}
