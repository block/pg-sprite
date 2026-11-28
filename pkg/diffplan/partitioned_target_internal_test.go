// White-box tests for the order of the partitioned-parent routing a
// desired-state plan applies.

package diffplan

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/verdict"
)

// A desired file that adds a foreign key to a partitioned table plans it as
// ADD ... NOT VALID, then VALIDATE. Before PostgreSQL 18 the server refuses
// NOT VALID on a partitioned table, so the plan must route the statement to
// the partitioned foreign key build before the partitioned-parent refusal
// sees it; in the other order the whole plan is refused. A shape the build
// does not carry, here an unnamed foreign key, keeps the NOT VALID steps
// and is refused as unsupported-partitioned-parent before anything runs.
func TestRoutePartitionedTargetRoutesAForeignKeyBeforeTheParentRefusal(t *testing.T) {
	addForeignKey := func(sql string) plan.Report {
		steps := []string{sql + ` NOT VALID`, `ALTER TABLE app.events VALIDATE CONSTRAINT events_account_fk`}
		return plan.Report{Disposition: router.DispositionExecute, Statements: []plan.Statement{{
			SQL:         sql,
			Route:       planner.RouteNative,
			Backend:     router.BackendNative,
			Disposition: router.DispositionExecute,
			Decisions: []planner.Decision{{
				Route: planner.RouteNative, Reason: planner.ReasonSaferIdiom,
				SaferSQL: steps, SaferSQLExecution: planner.ExecutionAutocommit,
			}},
			ExecSQL:   steps,
			Execution: planner.ExecutionAutocommit,
		}}}
	}

	t.Run("a foreign key the build carries runs as the build before 18", func(t *testing.T) {
		const sql = `ALTER TABLE app.events ADD CONSTRAINT events_account_fk FOREIGN KEY (account_id) REFERENCES app.accounts(id)`
		r := addForeignKey(sql)
		require.NoError(t, routePartitionedTarget(&r, 16))
		assert.Equal(t, router.DispositionExecute, r.Disposition)
		assert.Equal(t, []string{sql}, r.Statements[0].ExecSQL)
		assert.Equal(t, planner.ExecutionPartitionedForeignKeyBuild, r.Statements[0].Execution)
	})

	t.Run("a foreign key the build does not carry is refused before 18", func(t *testing.T) {
		const sql = `ALTER TABLE app.events ADD FOREIGN KEY (account_id) REFERENCES app.accounts(id)`
		r := addForeignKey(sql)
		require.NoError(t, routePartitionedTarget(&r, 16))
		assert.Equal(t, router.DispositionRefuse, r.Disposition)
		assert.Equal(t, verdict.ReasonUnsupportedPartitionedParent, r.Statements[0].Reason)
		assert.Equal(t, verdict.ClassEnvironmental, r.Statements[0].Class)
	})

	t.Run("on 18 the NOT VALID then VALIDATE steps run", func(t *testing.T) {
		const sql = `ALTER TABLE app.events ADD CONSTRAINT events_account_fk FOREIGN KEY (account_id) REFERENCES app.accounts(id)`
		r := addForeignKey(sql)
		require.NoError(t, routePartitionedTarget(&r, 18))
		assert.Equal(t, router.DispositionExecute, r.Disposition)
		assert.Equal(t, []string{sql + ` NOT VALID`, `ALTER TABLE app.events VALIDATE CONSTRAINT events_account_fk`}, r.Statements[0].ExecSQL)
		assert.Equal(t, planner.ExecutionAutocommit, r.Statements[0].Execution)
	})
}

// A desired file that adds a primary key to a partitioned table plans it as
// a plain table's concurrent unique build and USING INDEX attach, which the
// partitioned-parent refusal rejects. The plan must route the statement to
// the partitioned key build before that refusal sees it; without the key
// routing the whole plan is refused.
func TestRoutePartitionedTargetRoutesAKeyBeforeTheParentRefusal(t *testing.T) {
	const sql = `ALTER TABLE app.events ADD CONSTRAINT events_pkey PRIMARY KEY (id, created_at)`
	steps := []string{
		`CREATE UNIQUE INDEX CONCURRENTLY events_pkey ON app.events (id, created_at)`,
		`ALTER TABLE app.events ADD CONSTRAINT events_pkey PRIMARY KEY USING INDEX events_pkey`,
	}
	r := plan.Report{Disposition: router.DispositionExecute, Statements: []plan.Statement{{
		SQL:         sql,
		Route:       planner.RouteNative,
		Backend:     router.BackendNative,
		Disposition: router.DispositionExecute,
		Decisions: []planner.Decision{{
			Route: planner.RouteNative, Reason: planner.ReasonSaferIdiom,
			SaferSQL: steps, SaferSQLExecution: planner.ExecutionAutocommit,
		}},
		ExecSQL:   steps,
		Execution: planner.ExecutionAutocommit,
	}}}
	require.NoError(t, routePartitionedTarget(&r, 16))
	assert.Equal(t, router.DispositionExecute, r.Disposition)
	assert.Equal(t, []string{sql}, r.Statements[0].ExecSQL)
	assert.Equal(t, planner.ExecutionPartitionedKeyBuild, r.Statements[0].Execution)
}
