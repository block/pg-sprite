package diffplan_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/router"
)

// A desired file that adds columns to a table with one column number left
// plans the first add to run and refuses the second, since PostgreSQL
// would refuse it with the table's exclusive lock held. The table's
// dropped columns hold the numbers it has used.
func TestPlanRefusesAColumnAddPastTheColumnLimit(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.events (id bigint PRIMARY KEY);
		DO $$ BEGIN
			FOR i IN 1..1598 LOOP
				EXECUTE pg_catalog.format('ALTER TABLE %[1]s.events ADD COLUMN c%%s int', i);
				EXECUTE pg_catalog.format('ALTER TABLE %[1]s.events DROP COLUMN c%%s', i);
			END LOOP;
		END $$`, schema))
	require.NoError(t, err)

	report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{
		Schema:  schema,
		Desired: parseDesired(t, `CREATE TABLE events (id bigint PRIMARY KEY, kind text, note text);`),
	})
	require.NoError(t, err)
	assert.Equal(t, router.DispositionRefuse, report.Disposition)

	reasons := map[string]planner.Reason{}
	var refused planner.Decision
	for _, st := range report.Statements {
		for _, d := range st.Decisions {
			reasons[d.Operation] = d.Reason
			if d.Reason == planner.ReasonColumnLimit {
				refused = d
			}
		}
	}
	assert.Equal(t, map[string]planner.Reason{
		"ADD COLUMN kind": planner.ReasonMetadataOnly,
		"ADD COLUMN note": planner.ReasonColumnLimit,
	}, reasons)
	assert.Equal(t, &preflight.ColumnNumbers{
		Left:    1,
		Fullest: []preflight.FullTable{{Name: pgx.Identifier{schema, "events"}.Sanitize(), Root: true}},
	}, refused.ColumnLimit, "the refusal carries the numbers free before the plan, which the help offers")
	assert.True(t, strings.HasSuffix(planner.ColumnLimitHelp(refused.ColumnLimit), ", or add at most 1 column"))
}
