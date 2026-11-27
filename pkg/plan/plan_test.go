package plan_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/suggest"
	"github.com/block/pg-sprite/pkg/verdict"
)

func TestNewReportStampsVersionAndEmptyStatements(t *testing.T) {
	r := plan.NewReport(plan.SourceDiff)
	assert.Equal(t, plan.FormatVersion, r.FormatVersion)
	assert.Equal(t, plan.SourceDiff, r.Source)
	require.NotNil(t, r.Statements)
	assert.Empty(t, r.Statements)
}

func TestFromRoutedMapsEveryRoutedField(t *testing.T) {
	rs := router.Statement{
		Plan: planner.Plan{
			Statement: "CREATE INDEX i ON t (c)",
			Route:     planner.RouteNative,
			Decisions: []planner.Decision{{
				Operation: "create index",
				Route:     planner.RouteNative,
				Reason:    planner.ReasonSaferIdiom,
				SaferSQL:  []string{"CREATE INDEX CONCURRENTLY i ON t (c)"},
			}},
		},
		Backend:     router.BackendNative,
		Disposition: router.DispositionExecute,
		ExecSQL:     []string{"CREATE INDEX CONCURRENTLY i ON t (c)"},
	}
	st, err := plan.FromRouted(rs)
	require.NoError(t, err)
	assert.Equal(t, "CREATE INDEX i ON t (c)", st.SQL)
	assert.Equal(t, planner.RouteNative, st.Route)
	assert.Equal(t, router.BackendNative, st.Backend)
	assert.Equal(t, router.DispositionExecute, st.Disposition)
	assert.Equal(t, rs.Decisions, st.Decisions)
	assert.Equal(t, rs.ExecSQL, st.ExecSQL)
	assert.Equal(t, planner.ExecutionAutocommit, st.Execution,
		"exec_sql carries its execution contract")
	assert.False(t, st.Destructive, "no destructive decision means a non-destructive statement")
	assert.Empty(t, st.Guidance, "guidance is present exactly when the statement is rewrite-required")
}

// A rewrite-required statement carries the typed manual path derived from
// the operation the refusal is about — the same Guidance vocabulary the
// suggest report uses — so a consumer can render remediation without
// re-parsing the SQL.
func TestFromRoutedDerivesGuidanceForRewriteRequired(t *testing.T) {
	routed := router.Route([]planner.Plan{
		classifyCanonical(t, "ALTER TABLE users ADD COLUMN nickname text UNIQUE"),
	})
	require.Len(t, routed.Statements, 1)
	require.Equal(t, router.DispositionRewriteRequired, routed.Statements[0].Disposition,
		"an inline column constraint has no constructible online rewrite")

	st, err := plan.FromRouted(routed.Statements[0])
	require.NoError(t, err)
	assert.Equal(t, suggest.GuidanceAddColumnThenConstraint, st.Guidance)
}

// A rename is refused as rewrite-required and the plan names the manual path
// the application can follow: expand-and-contract for a column, a
// coordinated deploy for a table.
func TestFromRoutedDerivesGuidanceForAppBreakingRename(t *testing.T) {
	for name, tc := range map[string]struct {
		sql  string
		want suggest.Guidance
	}{
		"rename column": {"ALTER TABLE users RENAME COLUMN name TO full_name", suggest.GuidanceExpandContractColumn},
		"rename table":  {"ALTER TABLE users RENAME TO accounts", suggest.GuidanceCoordinatedTableRename},
	} {
		t.Run(name, func(t *testing.T) {
			routed := router.Route([]planner.Plan{classifyCanonical(t, tc.sql)})
			require.Len(t, routed.Statements, 1)
			require.Equal(t, router.DispositionRewriteRequired, routed.Statements[0].Disposition,
				"a rename breaks running application instances, so it never executes as submitted")

			st, err := plan.FromRouted(routed.Statements[0])
			require.NoError(t, err)
			assert.Equal(t, tc.want, st.Guidance)
		})
	}
}

// A compound rewrite-required statement advises splitting first: no safer
// sequence can be constructed for a multi-operation statement, so the
// manual path starts with one statement per operation.
func TestFromRoutedCompoundRewriteRequiredAdvisesSplit(t *testing.T) {
	routed := router.Route([]planner.Plan{
		classifyCanonical(t, "ALTER TABLE users ADD COLUMN nickname text UNIQUE, ADD COLUMN age int"),
	})
	require.Len(t, routed.Statements, 1)
	require.Equal(t, router.DispositionRewriteRequired, routed.Statements[0].Disposition)

	st, err := plan.FromRouted(routed.Statements[0])
	require.NoError(t, err)
	assert.Equal(t, suggest.GuidanceSplitStatement, st.Guidance)
}

// Destructive is derived from the classifier's decisions — one destructive
// operation makes the whole statement destructive — so both front doors
// report it identically by construction, whichever one built the report.
func TestFromRoutedDerivesDestructiveFromDecisions(t *testing.T) {
	rs := router.Statement{
		Plan: planner.Plan{
			Statement: "ALTER TABLE t ADD COLUMN c int, DROP COLUMN doomed",
			Route:     planner.RouteNative,
			Decisions: []planner.Decision{
				{Operation: "ADD COLUMN c", Route: planner.RouteNative, Reason: planner.ReasonMetadataOnly},
				{Operation: "DROP COLUMN doomed", Destructive: true, Route: planner.RouteNative, Reason: planner.ReasonMetadataOnly},
			},
		},
		Backend:     router.BackendNative,
		Disposition: router.DispositionExecute,
	}
	st, err := plan.FromRouted(rs)
	require.NoError(t, err)
	assert.True(t, st.Destructive,
		"one destructive decision makes the statement destructive")
	assert.Empty(t, st.Execution, "no exec_sql means no execution contract")
}

// A planner-level refusal carries the same typed reason the run path's
// refusal verdict reports, so a dry-run report and a run receipt for the
// same statement match on the typed field alone.
func TestFromRoutedStampsUnsupportedStatementOnRefusal(t *testing.T) {
	rs := router.Statement{
		Plan: planner.Plan{
			Statement: "ALTER TABLE t SET UNLOGGED",
			Route:     planner.RouteRefuse,
			Decisions: []planner.Decision{{
				Operation: "unrecognized operation",
				Route:     planner.RouteRefuse,
				Reason:    planner.ReasonUnsupportedOperation,
			}},
		},
		Disposition: router.DispositionRefuse,
	}
	st, err := plan.FromRouted(rs)
	require.NoError(t, err)
	assert.Equal(t, verdict.ReasonUnsupportedStatement, st.Reason)
}

func TestRefuseUnsupportedPartitionedParentWithdrawsExecutionAdvice(t *testing.T) {
	r := plan.Report{
		Disposition: router.DispositionExecute,
		Statements: []plan.Statement{{
			Route:       planner.RouteNative,
			Backend:     router.BackendNative,
			Disposition: router.DispositionExecute,
			Decisions: []planner.Decision{{
				Route: planner.RouteNative, Reason: planner.ReasonSaferIdiom,
				SaferSQL:          []string{"CREATE INDEX CONCURRENTLY i ON t (c)"},
				SaferSQLExecution: planner.ExecutionAutocommit,
			}},
			ExecSQL: []string{"CREATE INDEX CONCURRENTLY i ON t (c)"}, Execution: planner.ExecutionAutocommit,
		}},
	}
	require.NoError(t, plan.RefuseUnsupportedPartitionedParent(&r, []preflight.PartitionRefusalCause{preflight.PartitionCauseConcurrentIndexBuild}))

	assert.Equal(t, router.DispositionRefuse, r.Disposition)
	assert.Equal(t, verdict.ReasonUnsupportedPartitionedParent, r.Reason)
	assert.Equal(t, verdict.ClassCapabilityBoundary, r.Class)
	assert.Equal(t, planner.RouteNative, r.Statements[0].Route)
	assert.Equal(t, verdict.ReasonUnsupportedPartitionedParent, r.Statements[0].Reason)
	assert.Equal(t, verdict.ClassCapabilityBoundary, r.Statements[0].Class)
	assert.Empty(t, r.Statements[0].Owner)
	assert.Empty(t, r.Statements[0].Cause, "a target-facts refusal carries no create-shape cause")
	assert.Empty(t, r.Statements[0].Backend)
	assert.Empty(t, r.Statements[0].ExecSQL)
	assert.Empty(t, r.Statements[0].Execution)
	assert.Empty(t, r.Statements[0].Decisions[0].SaferSQL)
	assert.Empty(t, r.Statements[0].Decisions[0].SaferSQLExecution)
}

// A CREATE INDEX on a partitioned parent is routed to the partitioned index
// build, whatever form the planner carried: the one step is the statement
// itself, the decision's safer sequence is that statement, and the plan stays
// executable. A statement of another shape is refused by its cause.
func TestRoutePartitionedParentRoutesCreateIndexAndRefusesTheRest(t *testing.T) {
	const sql = "CREATE INDEX i ON s.p (c)"
	indexStatement := func() plan.Statement {
		return plan.Statement{
			SQL:         sql,
			Route:       planner.RouteNative,
			Backend:     router.BackendNative,
			Disposition: router.DispositionExecute,
			Decisions: []planner.Decision{{
				Route: planner.RouteNative, Reason: planner.ReasonSaferIdiom,
				SaferSQL:          []string{"CREATE INDEX CONCURRENTLY i ON s.p (c)"},
				SaferSQLExecution: planner.ExecutionAutocommit,
			}},
			ExecSQL:   []string{"CREATE INDEX CONCURRENTLY i ON s.p (c)"},
			Execution: planner.ExecutionAutocommit,
		}
	}

	t.Run("create index", func(t *testing.T) {
		r := plan.Report{Disposition: router.DispositionExecute, Statements: []plan.Statement{indexStatement()}}
		require.NoError(t, plan.RoutePartitionedParent(&r, 16))
		assert.Equal(t, router.DispositionExecute, r.Disposition)
		st := r.Statements[0]
		assert.Equal(t, router.DispositionExecute, st.Disposition)
		assert.Equal(t, []string{sql}, st.ExecSQL)
		assert.Equal(t, planner.ExecutionPartitionedIndexBuild, st.Execution)
		assert.Equal(t, []string{sql}, st.Decisions[0].SaferSQL)
		assert.Equal(t, planner.ExecutionPartitionedIndexBuild, st.Decisions[0].SaferSQLExecution)
		assert.Equal(t, planner.RouteNative, st.Route)
		assert.Equal(t, router.BackendNative, st.Backend)
	})

	t.Run("a decision with no safer sequence gains none", func(t *testing.T) {
		st := indexStatement()
		st.Decisions = []planner.Decision{{Route: planner.RouteNative, Reason: planner.ReasonMetadataOnly}}
		r := plan.Report{Disposition: router.DispositionExecute, Statements: []plan.Statement{st}}
		require.NoError(t, plan.RoutePartitionedParent(&r, 16))
		assert.Empty(t, r.Statements[0].Decisions[0].SaferSQL)
		assert.Empty(t, r.Statements[0].Decisions[0].SaferSQLExecution)
	})

	t.Run("index adoption stays refused", func(t *testing.T) {
		adopt := "ALTER TABLE s.p ADD CONSTRAINT p_pk PRIMARY KEY USING INDEX ix"
		r := plan.Report{Disposition: router.DispositionExecute, Statements: []plan.Statement{
			indexStatement(),
			{SQL: adopt, Route: planner.RouteNative, Backend: router.BackendNative,
				Disposition: router.DispositionExecute, ExecSQL: []string{adopt}, Execution: planner.ExecutionAutocommit},
		}}
		require.NoError(t, plan.RoutePartitionedParent(&r, 16))
		assert.Equal(t, router.DispositionRefuse, r.Disposition)
		assert.Equal(t, verdict.ReasonUnsupportedPartitionedParent, r.Reason)
		assert.Equal(t, verdict.ClassByDesign, r.Class)
		assert.Equal(t, router.DispositionExecute, r.Statements[0].Disposition, "the index statement keeps its route")
		assert.Equal(t, planner.ExecutionPartitionedIndexBuild, r.Statements[0].Execution)
		assert.Equal(t, router.DispositionRefuse, r.Statements[1].Disposition)
		assert.Empty(t, r.Statements[1].ExecSQL)
	})

	t.Run("a step that does not parse is an error", func(t *testing.T) {
		r := plan.Report{Statements: []plan.Statement{{ExecSQL: []string{"CREATE INDEX"}}}}
		require.Error(t, plan.RoutePartitionedParent(&r, 16))
	})
}

// An ADD FOREIGN KEY on a partitioned table, which the planner substitutes
// with a plain table's NOT VALID then VALIDATE, routes to the partitioned
// foreign key build before PostgreSQL 18: its one step becomes the statement
// and its execution partitioned-foreign-key-build, and RoutePartitionedParent
// then leaves it executable. Without the routing the same report is refused,
// which is what a declarative plan, which does not route foreign keys, still
// gets. On 18 nothing routes and the substitute runs as planned.
func TestRoutePartitionedForeignKeyBuildsRoutesAForeignKeyThatWouldBeRefused(t *testing.T) {
	const sql = `ALTER TABLE app.events ADD CONSTRAINT events_account_fk FOREIGN KEY (account_id) REFERENCES app.accounts (id)`
	steps := []string{
		`ALTER TABLE app.events ADD CONSTRAINT events_account_fk FOREIGN KEY (account_id) REFERENCES app.accounts (id) NOT VALID`,
		`ALTER TABLE app.events VALIDATE CONSTRAINT events_account_fk`,
	}
	fkReport := func() plan.Report {
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

	t.Run("routed before 18", func(t *testing.T) {
		r := fkReport()
		require.NoError(t, plan.RoutePartitionedForeignKeyBuilds(&r, 16))
		require.NoError(t, plan.RoutePartitionedParent(&r, 16))
		assert.Equal(t, router.DispositionExecute, r.Disposition)
		st := r.Statements[0]
		assert.Equal(t, router.DispositionExecute, st.Disposition)
		assert.Equal(t, []string{sql}, st.ExecSQL)
		assert.Equal(t, planner.ExecutionPartitionedForeignKeyBuild, st.Execution)
		assert.Equal(t, []string{sql}, st.Decisions[0].SaferSQL)
		assert.Equal(t, planner.ExecutionPartitionedForeignKeyBuild, st.Decisions[0].SaferSQLExecution)
	})

	// The imperative route does not gate on the build's shape admission:
	// the dry run runs that admission after the routing, so a shape the build
	// refuses meets the build's own refusal, which names the clause, instead
	// of the partitioned-parent refusal, which says to upgrade the server.
	t.Run("a shape the build refuses is still routed", func(t *testing.T) {
		const onDeleteColumns = `ALTER TABLE app.events ADD CONSTRAINT events_account_fk FOREIGN KEY (account_id, region) REFERENCES app.accounts (id, region) ON DELETE SET NULL (account_id)`
		r := fkReport()
		r.Statements[0].SQL = onDeleteColumns
		require.NoError(t, plan.RoutePartitionedForeignKeyBuilds(&r, 16))
		require.NoError(t, plan.RoutePartitionedParent(&r, 16))
		assert.Equal(t, router.DispositionExecute, r.Disposition)
		assert.Equal(t, []string{onDeleteColumns}, r.Statements[0].ExecSQL)
		assert.Equal(t, planner.ExecutionPartitionedForeignKeyBuild, r.Statements[0].Execution)
	})

	t.Run("not routed before 18 is refused", func(t *testing.T) {
		r := fkReport()
		require.NoError(t, plan.RoutePartitionedParent(&r, 16))
		assert.Equal(t, router.DispositionRefuse, r.Disposition)
		assert.Equal(t, verdict.ReasonUnsupportedPartitionedParent, r.Reason)
	})

	t.Run("on 18 the substitute runs", func(t *testing.T) {
		r := fkReport()
		require.NoError(t, plan.RoutePartitionedForeignKeyBuilds(&r, 18))
		require.NoError(t, plan.RoutePartitionedParent(&r, 18))
		assert.Equal(t, router.DispositionExecute, r.Disposition)
		assert.Equal(t, steps, r.Statements[0].ExecSQL)
		assert.Equal(t, planner.ExecutionAutocommit, r.Statements[0].Execution)
	})

	t.Run("a refused statement is left refused", func(t *testing.T) {
		r := fkReport()
		r.Statements[0].Disposition = router.DispositionRewriteRequired
		require.NoError(t, plan.RoutePartitionedForeignKeyBuilds(&r, 16))
		assert.Equal(t, planner.ExecutionAutocommit, r.Statements[0].Execution)
	})
}

// A desired-state plan routes a foreign key the file adds to a partitioned
// table to the same build before PostgreSQL 18, but only when the build's
// own admission takes its shape, because a desired run has no admission
// between the plan and the run: a plan that shows the build must be one the
// run executes as the build. A shape the build refuses, such as an ON DELETE
// column list, keeps the planner's NOT VALID step and stays refused.
func TestRouteDesiredPartitionedForeignKeyBuildsRoutesOnlyAnAdmittedShape(t *testing.T) {
	fkReport := func(sql string) plan.Report {
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

	t.Run("an admitted shape is routed before 18", func(t *testing.T) {
		const sql = `ALTER TABLE app.events ADD CONSTRAINT events_account_fk FOREIGN KEY (account_id) REFERENCES app.accounts(id)`
		r := fkReport(sql)
		require.NoError(t, plan.RouteDesiredPartitionedForeignKeyBuilds(&r, 16))
		require.NoError(t, plan.RoutePartitionedParent(&r, 16))
		assert.Equal(t, router.DispositionExecute, r.Disposition)
		assert.Equal(t, []string{sql}, r.Statements[0].ExecSQL)
		assert.Equal(t, planner.ExecutionPartitionedForeignKeyBuild, r.Statements[0].Execution)
	})

	t.Run("a shape the build refuses stays refused", func(t *testing.T) {
		const sql = `ALTER TABLE app.events ADD CONSTRAINT events_account_fk FOREIGN KEY (account_id, region) REFERENCES app.accounts(id, region) ON DELETE SET NULL (account_id)`
		r := fkReport(sql)
		require.NoError(t, plan.RouteDesiredPartitionedForeignKeyBuilds(&r, 16))
		require.NoError(t, plan.RoutePartitionedParent(&r, 16))
		assert.Equal(t, router.DispositionRefuse, r.Disposition)
		assert.Equal(t, verdict.ReasonUnsupportedPartitionedParent, r.Statements[0].Reason)
		assert.Equal(t, verdict.ClassEnvironmental, r.Statements[0].Class)
		assert.Empty(t, r.Statements[0].Execution, "a refusal carries nothing to run")
	})

	t.Run("on 18 the substitute runs", func(t *testing.T) {
		const sql = `ALTER TABLE app.events ADD CONSTRAINT events_account_fk FOREIGN KEY (account_id) REFERENCES app.accounts(id)`
		r := fkReport(sql)
		require.NoError(t, plan.RouteDesiredPartitionedForeignKeyBuilds(&r, 18))
		require.NoError(t, plan.RoutePartitionedParent(&r, 18))
		assert.Equal(t, router.DispositionExecute, r.Disposition)
		assert.Equal(t, planner.ExecutionAutocommit, r.Statements[0].Execution)
	})
}

// A diff-derived drop of a partitioned table's index stays refused, because
// PostgreSQL has no concurrent drop for it, and discloses the one drop there
// is: the plain DROP INDEX as its step, with the blocking-index-drop
// execution, so a reviewer sees the blocking form and the fingerprint pins it.
// An alter-source drop, which carries no reviewed definition, stays refused
// with nothing to run.
func TestRoutePartitionedParentDisclosesTheBlockingIndexDrop(t *testing.T) {
	const sql = "DROP INDEX s.p_c_idx"
	dropStatement := func() plan.Statement {
		return plan.Statement{
			SQL:         sql,
			Kind:        schemadiff.ChangeDropIndex,
			Destructive: true,
			Route:       planner.RouteNative,
			Backend:     router.BackendNative,
			Disposition: router.DispositionExecute,
			Decisions: []planner.Decision{{
				Route: planner.RouteNative, Reason: planner.ReasonSaferIdiom, Destructive: true,
				SaferSQL:          []string{"DROP INDEX CONCURRENTLY s.p_c_idx"},
				SaferSQLExecution: planner.ExecutionAutocommit,
			}},
			ExecSQL:   []string{"DROP INDEX CONCURRENTLY s.p_c_idx"},
			Execution: planner.ExecutionAutocommit,
			Drops: &schemadiff.DroppedObject{
				Kind: schemadiff.DroppedIndex, Name: "p_c_idx",
				Definition: "CREATE INDEX p_c_idx ON ONLY s.p USING btree (c)",
			},
		}
	}

	t.Run("a reviewed drop carries the plain drop", func(t *testing.T) {
		r := plan.Report{Disposition: router.DispositionExecute, Statements: []plan.Statement{dropStatement()}}
		require.NoError(t, plan.RoutePartitionedParent(&r, 16))
		st := r.Statements[0]
		assert.Equal(t, router.DispositionRefuse, st.Disposition)
		assert.Equal(t, verdict.ReasonUnsupportedPartitionedParent, st.Reason)
		assert.Equal(t, verdict.ClassByDesign, st.Class)
		require.NotNil(t, st.BlockingPassthroughEligible)
		assert.True(t, *st.BlockingPassthroughEligible)
		assert.Empty(t, st.Backend)
		assert.Equal(t, []string{sql}, st.ExecSQL, "the plain drop, never CONCURRENTLY")
		assert.Equal(t, planner.ExecutionBlockingIndexDrop, st.Execution)
		assert.Empty(t, st.Decisions[0].SaferSQL, "no concurrent idiom is offered")
		assert.Equal(t, router.DispositionRefuse, r.Disposition)
	})

	t.Run("a drop with no reviewed definition stays refused with nothing to run", func(t *testing.T) {
		st := dropStatement()
		st.Kind, st.Drops = "", nil
		r := plan.Report{Disposition: router.DispositionExecute, Statements: []plan.Statement{st}}
		require.NoError(t, plan.RoutePartitionedParent(&r, 16))
		assert.Equal(t, router.DispositionRefuse, r.Statements[0].Disposition)
		assert.Empty(t, r.Statements[0].ExecSQL)
		assert.Empty(t, r.Statements[0].Execution)
	})
}

// An ADD PRIMARY KEY on a partitioned table, which the planner substitutes
// with a plain table's concurrent unique build and USING INDEX attach, routes
// to the partitioned key build: its one step becomes the statement and its
// execution partitioned-key-build, and RoutePartitionedParent then leaves it
// executable. Without the key routing the same report is refused, which is
// what a caller that skips the key routing gets.
func TestRoutePartitionedKeyBuildsRoutesAKeyThatWouldBeRefused(t *testing.T) {
	const sql = "ALTER TABLE s.p ADD CONSTRAINT p_pkey PRIMARY KEY (id)"
	steps := []string{
		"CREATE UNIQUE INDEX CONCURRENTLY p_pkey ON s.p (id)",
		"ALTER TABLE s.p ADD CONSTRAINT p_pkey PRIMARY KEY USING INDEX p_pkey",
	}
	keyReport := func() plan.Report {
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

	t.Run("routed", func(t *testing.T) {
		r := keyReport()
		require.NoError(t, plan.RoutePartitionedKeyBuilds(&r))
		require.NoError(t, plan.RoutePartitionedParent(&r, 16))
		assert.Equal(t, router.DispositionExecute, r.Disposition)
		st := r.Statements[0]
		assert.Equal(t, router.DispositionExecute, st.Disposition)
		assert.Equal(t, []string{sql}, st.ExecSQL)
		assert.Equal(t, planner.ExecutionPartitionedKeyBuild, st.Execution)
		assert.Equal(t, []string{sql}, st.Decisions[0].SaferSQL)
		assert.Equal(t, planner.ExecutionPartitionedKeyBuild, st.Decisions[0].SaferSQLExecution)
	})

	t.Run("not routed", func(t *testing.T) {
		r := keyReport()
		require.NoError(t, plan.RoutePartitionedParent(&r, 16))
		assert.Equal(t, router.DispositionRefuse, r.Disposition)
		assert.Equal(t, verdict.ReasonUnsupportedPartitionedParent, r.Reason)
	})

	t.Run("a refused statement is left refused", func(t *testing.T) {
		r := keyReport()
		r.Statements[0].Disposition = router.DispositionRewriteRequired
		require.NoError(t, plan.RoutePartitionedKeyBuilds(&r))
		assert.Equal(t, planner.ExecutionAutocommit, r.Statements[0].Execution)
	})
}

// A desired-state plan runs no admission between the plan and the run, so
// its router sends a key to the build only when the build's shape check
// accepts it. A key the build would refuse keeps the planner's USING INDEX
// steps and meets the partitioned-parent refusal.
func TestRouteDesiredPartitionedKeyBuildsRoutesOnlyAdmittedShapes(t *testing.T) {
	keyReport := func(sql string) plan.Report {
		steps := []string{
			"CREATE UNIQUE INDEX CONCURRENTLY events_key ON app.events (id, created_at)",
			"ALTER TABLE app.events ADD CONSTRAINT events_key UNIQUE USING INDEX events_key",
		}
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

	t.Run("a named primary key routes to the build", func(t *testing.T) {
		const sql = `ALTER TABLE app.events ADD CONSTRAINT events_pkey PRIMARY KEY (id, created_at)`
		r := keyReport(sql)
		require.NoError(t, plan.RouteDesiredPartitionedKeyBuilds(&r))
		require.NoError(t, plan.RoutePartitionedParent(&r, 16))
		assert.Equal(t, router.DispositionExecute, r.Disposition)
		st := r.Statements[0]
		assert.Equal(t, []string{sql}, st.ExecSQL)
		assert.Equal(t, planner.ExecutionPartitionedKeyBuild, st.Execution)
		assert.Equal(t, []string{sql}, st.Decisions[0].SaferSQL)
		assert.Equal(t, planner.ExecutionPartitionedKeyBuild, st.Decisions[0].SaferSQLExecution)
	})

	// The build gives each partition a constraint it cannot make
	// deferrable, so it refuses DEFERRABLE; the plan keeps the refusal.
	t.Run("a deferrable unique constraint stays refused", func(t *testing.T) {
		r := keyReport(`ALTER TABLE app.events ADD CONSTRAINT events_key UNIQUE (id, created_at) DEFERRABLE`)
		require.NoError(t, plan.RouteDesiredPartitionedKeyBuilds(&r))
		assert.Equal(t, planner.ExecutionAutocommit, r.Statements[0].Execution)
		require.NoError(t, plan.RoutePartitionedParent(&r, 16))
		assert.Equal(t, router.DispositionRefuse, r.Disposition)
		assert.Equal(t, verdict.ReasonUnsupportedPartitionedParent, r.Reason)
	})

	// The build names each partition's constraint after the parent's, so it
	// refuses an unnamed key. The diff names every key it plans, so this
	// shape reaches the router only from a caller-built report.
	t.Run("an unnamed key stays refused", func(t *testing.T) {
		r := keyReport(`ALTER TABLE app.events ADD UNIQUE (id, created_at)`)
		require.NoError(t, plan.RouteDesiredPartitionedKeyBuilds(&r))
		require.NoError(t, plan.RoutePartitionedParent(&r, 16))
		assert.Equal(t, router.DispositionRefuse, r.Disposition)
		assert.Equal(t, verdict.ReasonUnsupportedPartitionedParent, r.Reason)
	})

	// The imperative router routes the same unnamed key, because the
	// imperative dry run then runs the build's admission and refuses it with
	// the build's own reason.
	t.Run("the imperative router routes the unnamed key for its admission", func(t *testing.T) {
		r := keyReport(`ALTER TABLE app.events ADD UNIQUE (id, created_at)`)
		require.NoError(t, plan.RoutePartitionedKeyBuilds(&r))
		assert.Equal(t, planner.ExecutionPartitionedKeyBuild, r.Statements[0].Execution)
	})
}

// A desired file that adds a column beside a key plans both. Either router
// sends the key to the build and leaves the column add on its own step,
// since only a lone ADD PRIMARY KEY or ADD UNIQUE is the build's statement.
func TestKeyRoutingLeavesOtherStatementsAlone(t *testing.T) {
	executable := func(sql string) plan.Statement {
		return plan.Statement{
			SQL:         sql,
			Route:       planner.RouteNative,
			Backend:     router.BackendNative,
			Disposition: router.DispositionExecute,
			ExecSQL:     []string{sql},
			Execution:   planner.ExecutionAutocommit,
		}
	}
	for name, route := range map[string]func(*plan.Report) error{
		"desired":    plan.RouteDesiredPartitionedKeyBuilds,
		"imperative": plan.RoutePartitionedKeyBuilds,
	} {
		t.Run(name, func(t *testing.T) {
			r := plan.Report{Disposition: router.DispositionExecute, Statements: []plan.Statement{
				executable(`ALTER TABLE app.events ADD COLUMN note text`),
				executable(`ALTER TABLE app.events ADD CONSTRAINT events_pkey PRIMARY KEY (id, created_at)`),
			}}
			require.NoError(t, route(&r))
			assert.Equal(t, planner.ExecutionAutocommit, r.Statements[0].Execution, "the column add is not the key build")
			assert.Equal(t, planner.ExecutionPartitionedKeyBuild, r.Statements[1].Execution)
		})
	}
}

// The partition refusal's class is a property of the cause: an index-adoption
// refusal is by-design, a pre-18 NOT VALID foreign key is environmental. A
// registry keyed on the reason alone could not tell them apart.
func TestRefuseUnsupportedPartitionedParentClassFollowsCause(t *testing.T) {
	for cause, want := range map[preflight.PartitionRefusalCause]verdict.Class{
		preflight.PartitionCauseBlockingIndexBuild: verdict.ClassCapabilityBoundary,
		preflight.PartitionCauseIndexAdoption:      verdict.ClassByDesign,
		preflight.PartitionCauseNotValidForeignKey: verdict.ClassEnvironmental,
	} {
		r := plan.Report{Disposition: router.DispositionExecute, Statements: []plan.Statement{
			{Disposition: router.DispositionExecute},
			{Disposition: router.DispositionExecute},
		}}
		require.NoError(t, plan.RefuseUnsupportedPartitionedParent(&r, []preflight.PartitionRefusalCause{"", cause}))
		assert.Equal(t, router.DispositionExecute, r.Statements[0].Disposition, cause)
		assert.Empty(t, r.Statements[0].Class, cause)
		assert.Equal(t, want, r.Statements[1].Class, cause)
		assert.Equal(t, want, r.Class, "the report carries the first refusal's class")
	}
}

func TestRefusedStatementsReportAcceptedBlockingEligibility(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause preflight.PartitionRefusalCause
		want  bool
	}{
		{"blocking parent index", preflight.PartitionCauseBlockingIndexBuild, true},
		{"concurrent parent index", preflight.PartitionCauseConcurrentIndexBuild, false},
		{"concurrent parent index drop", preflight.PartitionCauseConcurrentIndexDrop, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := plan.NewReport(plan.SourceAlter)
			r.Statements = []plan.Statement{{Disposition: router.DispositionExecute}}
			require.NoError(t, plan.RefuseUnsupportedPartitionedParent(&r, []preflight.PartitionRefusalCause{tc.cause}))
			require.NotNil(t, r.Statements[0].BlockingPassthroughEligible)
			assert.Equal(t, tc.want, *r.Statements[0].BlockingPassthroughEligible)
			raw, err := json.Marshal(r.Statements[0])
			require.NoError(t, err)
			assert.Contains(t, string(raw), fmt.Sprintf(`"blocking_passthrough_eligible":%t`, tc.want))
		})
	}

	raw, err := json.Marshal(plan.Statement{Disposition: router.DispositionExecute})
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "blocking_passthrough_eligible")
}

// The refusal proof carries the preflight cause under the verdict package's
// own typed vocabulary. The two constant sets spell the same wire tokens, and
// this pins that correspondence for every cause preflight registers: a cause
// added on one side without the other is a registry hole, not a silent
// no-cause refusal.
func TestPartitionRefusalCarriesItsCause(t *testing.T) {
	for _, cause := range preflight.PartitionRefusalCauses() {
		r, ok := plan.PartitionRefusal(cause)
		require.True(t, ok, cause)
		assert.Equal(t, string(cause), string(r.Cause()), cause)
	}
}

func TestRefuseUnsupportedPartitionedParentFailsClosed(t *testing.T) {
	r := plan.Report{Disposition: router.DispositionExecute, Statements: []plan.Statement{{Disposition: router.DispositionExecute}}}
	require.Error(t, plan.RefuseUnsupportedPartitionedParent(&r, nil), "positional length mismatch")
	err := plan.RefuseUnsupportedPartitionedParent(&r, []preflight.PartitionRefusalCause{"parent-unknown-shape"})
	require.ErrorIs(t, err, executor.ErrInvariantViolation)
	assert.Equal(t, router.DispositionExecute, r.Statements[0].Disposition, "nothing is marked when the refusal cannot be classified")
	assert.Empty(t, r.Reason)
}

func TestRefuseUnsupportedCreateShapeMarksPositions(t *testing.T) {
	r := plan.Report{
		Disposition: router.DispositionExecute,
		Statements: []plan.Statement{
			{Backend: router.BackendNative, Disposition: router.DispositionExecute, ExecSQL: []string{"CREATE TABLE app.t (id int)"}, Execution: planner.ExecutionAutocommit},
			{Backend: router.BackendNative, Disposition: router.DispositionExecute, ExecSQL: []string{"CREATE INDEX i ON app.t (id)"}, Execution: planner.ExecutionAutocommit},
		},
	}
	require.NoError(t, plan.RefuseUnsupportedCreateShape(&r, []error{
		&executor.CreateShapeError{Cause: executor.CreateShapePartitionOf}, nil,
	}))

	assert.Equal(t, router.DispositionRefuse, r.Disposition)
	assert.Equal(t, verdict.ReasonUnsupportedStatement, r.Reason)
	assert.Equal(t, router.DispositionRefuse, r.Statements[0].Disposition)
	assert.Equal(t, verdict.ReasonUnsupportedStatement, r.Statements[0].Reason)
	assert.Equal(t, executor.CreateShapePartitionOf, r.Statements[0].Cause)
	assert.Empty(t, r.Statements[0].Backend)
	assert.Empty(t, r.Statements[0].ExecSQL)
	assert.Equal(t, router.DispositionExecute, r.Statements[1].Disposition)
	assert.Equal(t, router.BackendNative, r.Statements[1].Backend)
	assert.Empty(t, r.Statements[1].Cause)
	assert.NotEmpty(t, r.Statements[1].ExecSQL)
}

// The cause is read through the wrappers the executor puts around a
// shape refusal, so a *SequenceStepError naming the failed step still
// stamps the typed cause.
func TestRefuseUnsupportedCreateShapeReadsWrappedCause(t *testing.T) {
	r := plan.Report{
		Disposition: router.DispositionExecute,
		Statements: []plan.Statement{
			{Backend: router.BackendNative, Disposition: router.DispositionExecute, ExecSQL: []string{"CREATE TABLE app.t (id int)"}, Execution: planner.ExecutionAutocommit},
		},
	}
	wrapped := fmt.Errorf("desired statement 1 of 1: %w", &executor.CreateShapeError{Cause: executor.CreateShapeIfNotExists})
	require.NoError(t, plan.RefuseUnsupportedCreateShape(&r, []error{wrapped}))

	assert.Equal(t, executor.CreateShapeIfNotExists, r.Statements[0].Cause)
}

// A refusal the cause vocabulary does not name would leave the report with
// a refusal it cannot explain; the mutator fails closed and marks nothing.
func TestRefuseUnsupportedCreateShapeRejectsRefusalWithoutCause(t *testing.T) {
	r := plan.Report{
		Disposition: router.DispositionExecute,
		Statements: []plan.Statement{
			{Backend: router.BackendNative, Disposition: router.DispositionExecute, ExecSQL: []string{"CREATE TABLE app.t (id int)"}, Execution: planner.ExecutionAutocommit},
			{Backend: router.BackendNative, Disposition: router.DispositionExecute, ExecSQL: []string{"CREATE INDEX i ON app.t (id)"}, Execution: planner.ExecutionAutocommit},
		},
	}
	err := plan.RefuseUnsupportedCreateShape(&r, []error{
		&executor.CreateShapeError{Cause: executor.CreateShapePartitionOf}, errors.New("refused"),
	})

	require.ErrorIs(t, err, executor.ErrInvariantViolation,
		"a refusal the cause vocabulary cannot name fails closed as an invariant violation")
	assert.Equal(t, router.DispositionExecute, r.Disposition)
	for i := range r.Statements {
		assert.Equal(t, router.DispositionExecute, r.Statements[i].Disposition, "statement %d", i+1)
		assert.Empty(t, r.Statements[i].Cause, "statement %d", i+1)
		assert.NotEmpty(t, r.Statements[i].ExecSQL, "statement %d", i+1)
	}
}

func TestRefuseUnsupportedCreateShapePreservesExistingStatementReason(t *testing.T) {
	r := plan.Report{
		Disposition: router.DispositionRefuse,
		Reason:      verdict.ReasonUnsupportedPartitionedParent,
		Statements: []plan.Statement{{
			Backend:     router.BackendNative,
			Disposition: router.DispositionRefuse,
			Reason:      verdict.ReasonUnsupportedPartitionedParent,
			ExecSQL:     []string{"CREATE TABLE app.t (id int)"},
			Execution:   planner.ExecutionAutocommit,
		}},
	}
	require.NoError(t, plan.RefuseUnsupportedCreateShape(&r, []error{
		&executor.CreateShapeError{Cause: executor.CreateShapePartitionOf},
	}))

	assert.Equal(t, router.DispositionRefuse, r.Disposition)
	assert.Equal(t, verdict.ReasonUnsupportedPartitionedParent, r.Reason)
	assert.Equal(t, verdict.ReasonUnsupportedPartitionedParent, r.Statements[0].Reason)
	assert.Empty(t, r.Statements[0].Cause, "the cause belongs to the reason it explains; the earlier refusal keeps both")
	assert.Empty(t, r.Statements[0].Backend)
	assert.Empty(t, r.Statements[0].ExecSQL)
	assert.Empty(t, r.Statements[0].Execution)
}

// A refusal slice that does not line up with the planned statements means
// the shape check and the plan disagree about what the plan contains; no
// positional marking is safe, so the report is left as it was.
func TestRefuseUnsupportedCreateShapeRejectsLengthMismatch(t *testing.T) {
	r := plan.Report{
		Disposition: router.DispositionExecute,
		Statements: []plan.Statement{
			{Backend: router.BackendNative, Disposition: router.DispositionExecute, ExecSQL: []string{"CREATE TABLE app.t (id int)"}, Execution: planner.ExecutionAutocommit},
			{Backend: router.BackendNative, Disposition: router.DispositionExecute, ExecSQL: []string{"CREATE INDEX i ON app.t (id)"}, Execution: planner.ExecutionAutocommit},
		},
	}
	err := plan.RefuseUnsupportedCreateShape(&r, []error{
		&executor.CreateShapeError{Cause: executor.CreateShapePartitionOf},
	})

	require.Error(t, err)
	assert.Equal(t, router.DispositionExecute, r.Disposition)
	for i := range r.Statements {
		assert.Equal(t, router.DispositionExecute, r.Statements[i].Disposition, "statement %d", i+1)
		assert.NotEmpty(t, r.Statements[i].ExecSQL, "statement %d", i+1)
	}
}

func TestDiscloseGreenfieldExecutionUsesPlainSQLForExecutableStatements(t *testing.T) {
	tableExists := false
	concurrent := "CREATE INDEX CONCURRENTLY i ON s.t (c)"
	saferIdiom := func() []planner.Decision {
		return []planner.Decision{{
			Route:             planner.RouteNative,
			Reason:            planner.ReasonSaferIdiom,
			SaferSQL:          []string{concurrent},
			SaferSQLExecution: planner.ExecutionAutocommit,
		}}
	}
	r := plan.Report{TableExists: &tableExists, Statements: []plan.Statement{
		{
			SQL:         "CREATE INDEX i ON s.t USING btree (c)",
			Disposition: router.DispositionExecute,
			ExecSQL:     []string{concurrent},
			Execution:   planner.ExecutionAutocommit,
			Decisions:   saferIdiom(),
		},
		{
			SQL:         "ALTER TABLE s.t SET UNLOGGED",
			Disposition: router.DispositionRefuse,
			ExecSQL:     []string{concurrent},
			Execution:   planner.ExecutionAutocommit,
			Decisions:   saferIdiom(),
		},
		{
			SQL:         "ALTER TABLE s.t ADD COLUMN nickname text UNIQUE",
			Disposition: router.DispositionRewriteRequired,
			Decisions:   saferIdiom(),
		},
		{
			SQL:         "ALTER TABLE s.t ALTER COLUMN c TYPE bigint",
			Disposition: router.DispositionUnavailable,
			Decisions:   saferIdiom(),
		},
		{
			SQL:         "CREATE TABLE s.t (c int)",
			Disposition: router.DispositionExecute,
			ExecSQL:     []string{"CREATE TABLE s.t (c int)"},
			Execution:   planner.ExecutionAutocommit,
			Decisions: []planner.Decision{{
				Route:  planner.RouteNative,
				Reason: planner.ReasonMetadataOnly,
			}},
		},
	}}

	plan.DiscloseGreenfieldExecution(&r)

	executable := r.Statements[0]
	assert.Equal(t, []string{executable.SQL}, executable.ExecSQL)
	assert.Equal(t, planner.ExecutionAutocommit, executable.Execution)
	assert.Equal(t, planner.ReasonMetadataOnly, executable.Decisions[0].Reason,
		"a safer-idiom build on a table born in this run is reclassified metadata-only")
	assert.Empty(t, executable.Decisions[0].SaferSQL)
	assert.Empty(t, executable.Decisions[0].SaferSQLExecution)

	for _, untouched := range r.Statements[1:4] {
		assert.Equal(t, saferIdiom(), untouched.Decisions, untouched.Disposition)
	}
	assert.Equal(t, []string{concurrent}, r.Statements[1].ExecSQL)
	assert.Equal(t, planner.ExecutionAutocommit, r.Statements[1].Execution)

	createTable := r.Statements[4]
	assert.Equal(t, []string{createTable.SQL}, createTable.ExecSQL)
	assert.Equal(t, planner.ExecutionAutocommit, createTable.Execution)
	assert.Equal(t, planner.ReasonMetadataOnly, createTable.Decisions[0].Reason)
}

func TestDiscloseGreenfieldExecutionRequiresAbsentTable(t *testing.T) {
	trueValue := true
	for _, tt := range []struct {
		name        string
		tableExists *bool
	}{
		{name: "table exists", tableExists: &trueValue},
		{name: "table existence unknown"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := plan.Report{
				TableExists: tt.tableExists,
				Statements: []plan.Statement{{
					SQL:         "CREATE INDEX i ON s.t (c)",
					Disposition: router.DispositionExecute,
					ExecSQL:     []string{"CREATE INDEX CONCURRENTLY i ON s.t (c)"},
					Execution:   planner.ExecutionAutocommit,
					Decisions: []planner.Decision{{
						Reason:            planner.ReasonSaferIdiom,
						SaferSQL:          []string{"CREATE INDEX CONCURRENTLY i ON s.t (c)"},
						SaferSQLExecution: planner.ExecutionAutocommit,
					}},
				}},
			}
			before, err := json.Marshal(r)
			require.NoError(t, err)

			plan.DiscloseGreenfieldExecution(&r)

			after, err := json.Marshal(r)
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
}

// The JSON shape is the adapter-facing contract: exact keys, exact
// omissions. A consumer pins format_version 28 against this test.
func TestReportJSONShape(t *testing.T) {
	exists := true
	r := plan.Report{
		FormatVersion: plan.FormatVersion,
		Source:        plan.SourceDiff,
		Schema:        "public",
		Table:         "t",
		ServerVersion: "16.4",
		TableExists:   &exists,
		Disposition:   router.DispositionRefuse,
		Statements: []plan.Statement{
			{
				SQL:         "DROP INDEX t_c_idx",
				Kind:        schemadiff.ChangeDropIndex,
				Destructive: true,
				Route:       planner.RouteNative,
				Backend:     router.BackendNative,
				Disposition: router.DispositionExecute,
				Decisions: []planner.Decision{{
					Operation:   "drop index",
					Destructive: true,
					Route:       planner.RouteNative,
					Reason:      planner.ReasonSaferIdiom,
				}},
				ExecSQL:   []string{"DROP INDEX CONCURRENTLY t_c_idx"},
				Execution: planner.ExecutionAutocommit,
			},
			{
				SQL:         "ALTER TABLE t ADD COLUMN nickname text UNIQUE",
				Route:       planner.RouteNative,
				Backend:     router.BackendNative,
				Disposition: router.DispositionRewriteRequired,
				Decisions: []planner.Decision{{
					Operation: "ADD COLUMN nickname",
					Route:     planner.RouteNative,
					Reason:    planner.ReasonSaferIdiom,
				}},
				Guidance: suggest.GuidanceAddColumnThenConstraint,
			},
			{
				SQL:         "ALTER TABLE t NO SUCH THING",
				Route:       planner.RouteRefuse,
				Disposition: router.DispositionRefuse,
				Decisions: []planner.Decision{{
					Operation: "unrecognized",
					Route:     planner.RouteRefuse,
					Reason:    planner.ReasonUnsupportedOperation,
				}},
			},
		},
	}
	r.Fingerprint = plan.Fingerprint(r.Statements)
	raw, err := json.Marshal(r)
	require.NoError(t, err)
	assert.JSONEq(t, fmt.Sprintf(`{
		"format_version": 28,
		"source": "diff",
		"schema": "public",
		"table": "t",
		"server_version": "16.4",
		"table_exists": true,
		"disposition": "refuse",
		"fingerprint": %q,
		"statements": [
			{
				"sql": "DROP INDEX t_c_idx",
				"kind": "drop-index",
				"destructive": true,
				"route": "native",
				"backend": "native",
				"disposition": "execute",
				"decisions": [
					{"operation": "drop index", "destructive": true, "route": "native", "reason": "safer-idiom"}
				],
				"exec_sql": ["DROP INDEX CONCURRENTLY t_c_idx"],
				"execution": "autocommit-each-step"
			},
			{
				"sql": "ALTER TABLE t ADD COLUMN nickname text UNIQUE",
				"destructive": false,
				"route": "native",
				"backend": "native",
				"disposition": "rewrite-required",
				"decisions": [
					{"operation": "ADD COLUMN nickname", "destructive": false, "route": "native", "reason": "safer-idiom"}
				],
				"guidance": "add-column-then-constraint"
			},
			{
				"sql": "ALTER TABLE t NO SUCH THING",
				"destructive": false,
				"route": "refuse",
				"disposition": "refuse",
				"decisions": [
					{"operation": "unrecognized", "destructive": false, "route": "refuse", "reason": "unsupported-operation"}
				]
			}
		]
	}`, r.Fingerprint), string(raw))
}

// Optional envelope fields are omitted, not emitted as zero values: a
// report that never introspected a target has no table_exists, and an
// empty plan serializes
// its statements as []. The fingerprint is never optional — an empty plan
// still has a defined identity.
func TestReportJSONOmitsUnsetOptionalFields(t *testing.T) {
	r := plan.NewReport(plan.SourceAlter)
	r.Disposition = router.DispositionExecute
	r.Fingerprint = plan.Fingerprint(r.Statements)
	raw, err := json.Marshal(r)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"format_version": 28,
		"source": "alter",
		"disposition": "execute",
		"fingerprint": "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"statements": []
	}`, string(raw))
}

// The report-level advisories ride alongside an empty statement list with
// exact keys: a consumer reading "nothing to run" must also see what the
// diff declined to do. Absent — not an empty array — when there is nothing
// to report (TestReportJSONOmitsUnsetOptionalFields).
func TestReportJSONAdvisoriesShape(t *testing.T) {
	exists := true
	r := plan.NewReport(plan.SourceDiff)
	r.Schema, r.Table, r.ServerVersion = "public", "t", "16.4"
	r.TableExists = &exists
	r.Disposition = router.DispositionExecute
	r.Advisories = []schemadiff.Advisory{
		{Kind: schemadiff.AdvisoryInvalidIndexUndeclared, Index: "t_c_idx_ccnew"},
	}
	r.Fingerprint = plan.Fingerprint(r.Statements)
	raw, err := json.Marshal(r)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"format_version": 28,
		"source": "diff",
		"schema": "public",
		"table": "t",
		"server_version": "16.4",
		"table_exists": true,
		"disposition": "execute",
		"fingerprint": "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"statements": [],
		"advisories": [
			{"kind": "invalid-index-undeclared", "index": "t_c_idx_ccnew"}
		]
	}`, string(raw))
}

// The statement-level cause is emitted only when a greenfield create-shape
// refusal set it; every other statement omits the key rather than
// serializing an empty string.
func TestStatementJSONCauseIsOptional(t *testing.T) {
	refused := plan.Statement{
		SQL:         "CREATE TABLE app.t PARTITION OF app.parent FOR VALUES IN (1)",
		Route:       planner.RouteRefuse,
		Disposition: router.DispositionRefuse,
		Reason:      verdict.ReasonUnsupportedStatement,
		Cause:       executor.CreateShapePartitionOf,
	}
	raw, err := json.Marshal(refused)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"cause":"partition-of"`)

	plain := plan.Statement{
		SQL:         "CREATE TABLE app.t (id int)",
		Route:       planner.RouteNative,
		Backend:     router.BackendNative,
		Disposition: router.DispositionExecute,
	}
	raw, err = json.Marshal(plain)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"cause"`)
}

// The fingerprint serialization is a pinned contract: a fixed statement
// list must hash to this exact digest. If this test fails, the identity
// definition changed and format_version must be bumped.
func TestFingerprintPinnedDigest(t *testing.T) {
	st := plan.Statement{
		SQL:         "ALTER TABLE t DROP COLUMN doomed",
		Route:       planner.RouteNative,
		Backend:     router.BackendNative,
		Disposition: router.DispositionExecute,
	}
	assert.Equal(t,
		"sha256:ec28ea60dfb1894212aa7dbe52ee355ec44b5abd94d0dac550c8e1e64dd76da0",
		plan.Fingerprint([]plan.Statement{st}))
}

// The fingerprint covers what would execute and only that: explanatory
// fields do not change identity; routing, order, and exec_sql do.
func TestFingerprintCoversExecutionNotExplanation(t *testing.T) {
	a := plan.Statement{SQL: "ALTER TABLE t ADD COLUMN c int", Route: planner.RouteNative,
		Backend: router.BackendNative, Disposition: router.DispositionExecute}
	b := plan.Statement{SQL: "ALTER TABLE t DROP COLUMN doomed", Route: planner.RouteNative,
		Backend: router.BackendNative, Disposition: router.DispositionExecute}
	base := plan.Fingerprint([]plan.Statement{a, b})

	explained := a
	explained.Destructive = true
	explained.Kind = schemadiff.ChangeAddColumn
	explained.Decisions = []planner.Decision{{Operation: "ADD COLUMN c", Route: planner.RouteNative}}
	explained.Guidance = suggest.GuidanceSplitStatement
	explained.Reason = verdict.ReasonUnsupportedStatement
	explained.Cause = executor.CreateShapePartitionOf
	assert.Equal(t, base, plan.Fingerprint([]plan.Statement{explained, b}),
		"decisions, kind, destructive, guidance, reason, and cause are explanatory: identity unchanged")

	assert.NotEqual(t, base, plan.Fingerprint([]plan.Statement{b, a}),
		"statement order is part of identity")

	rerouted := a
	rerouted.Route = planner.RouteCopyAndSwap
	rerouted.Backend = router.BackendCopyAndSwap
	assert.NotEqual(t, base, plan.Fingerprint([]plan.Statement{rerouted, b}),
		"a rerouted statement is a different plan")

	rewritten := a
	rewritten.ExecSQL = []string{"CREATE INDEX CONCURRENTLY i ON t (c)"}
	assert.NotEqual(t, base, plan.Fingerprint([]plan.Statement{rewritten, b}),
		"exec_sql is what runs: identity changes with it")
}

// An ADD FOREIGN KEY that runs as the partitioned foreign key build is not
// the same plan as the statement run literally, which validates every
// partition while it blocks writes to the whole tree.
func TestFingerprintCoversPartitionedForeignKeyBuildExecution(t *testing.T) {
	const sql = `ALTER TABLE app.events ADD CONSTRAINT events_account_fk FOREIGN KEY (account_id) REFERENCES app.accounts (id)`
	literal := plan.Statement{
		SQL:         sql,
		ExecSQL:     []string{sql},
		Route:       planner.RouteNative,
		Backend:     router.BackendNative,
		Disposition: router.DispositionExecute,
	}
	partitioned := literal
	partitioned.Execution = planner.ExecutionPartitionedForeignKeyBuild

	assert.NotEqual(t,
		plan.Fingerprint([]plan.Statement{literal}),
		plan.Fingerprint([]plan.Statement{partitioned}),
		"the partitioned foreign key build is not the literal ADD FOREIGN KEY")
}

// A CREATE INDEX on a partitioned table runs as the partition-by-partition
// build, whose one step is the statement as written. The same text on a plain
// table runs as a literal CREATE INDEX CONCURRENTLY, so the two are different
// plans and must not share a fingerprint, or an approver's pin could not tell
// them apart. A statement with any other execution keeps the identity it has
// always had, which TestFingerprintPinnedDigest also pins.
func TestFingerprintCoversPartitionedIndexBuildExecution(t *testing.T) {
	literal := plan.Statement{
		SQL:         "CREATE INDEX CONCURRENTLY i ON public.t (c)",
		ExecSQL:     []string{"CREATE INDEX CONCURRENTLY i ON public.t (c)"},
		Route:       planner.RouteNative,
		Backend:     router.BackendNative,
		Disposition: router.DispositionExecute,
	}
	partitioned := literal
	partitioned.Execution = planner.ExecutionPartitionedIndexBuild

	assert.NotEqual(t,
		plan.Fingerprint([]plan.Statement{literal}),
		plan.Fingerprint([]plan.Statement{partitioned}),
		"the partitioned build is not the literal concurrent build")

	autocommit := literal
	autocommit.Execution = planner.ExecutionAutocommit
	assert.Equal(t,
		plan.Fingerprint([]plan.Statement{literal}),
		plan.Fingerprint([]plan.Statement{autocommit}),
		"an autocommit execution is not hashed, so dry-run and run fingerprints that differ only in it agree")
}

// A reviewed drop of a partitioned table's index pins the blocking form it
// runs as: the same refused statement with no step to run, or with the
// plain drop as an ordinary step, is a different plan, so a pin taken on one
// cannot run the other.
func TestFingerprintCoversBlockingIndexDropExecution(t *testing.T) {
	refused := plan.Statement{
		SQL:         "DROP INDEX app.events_kind_idx",
		Route:       planner.RouteNative,
		Disposition: router.DispositionRefuse,
		Drops: &schemadiff.DroppedObject{
			Kind: schemadiff.DroppedIndex, Name: "events_kind_idx",
			Definition: "CREATE INDEX events_kind_idx ON ONLY app.events USING btree (kind)",
		},
	}
	blocking := refused
	blocking.ExecSQL = []string{refused.SQL}
	blocking.Execution = planner.ExecutionBlockingIndexDrop
	autocommit := blocking
	autocommit.Execution = planner.ExecutionAutocommit

	pinned := plan.Fingerprint([]plan.Statement{blocking})
	assert.NotEqual(t, pinned, plan.Fingerprint([]plan.Statement{refused}),
		"the refusal with no step to run is not the blocking drop")
	assert.NotEqual(t, pinned, plan.Fingerprint([]plan.Statement{autocommit}),
		"the plain drop as an ordinary step is not the blocking drop")
}

// What a drop removes is part of the plan's identity, name and definition
// and, for a column, what goes with it: a name that now stands for another
// object is another plan.
func TestFingerprintCoversWhatADropRemoves(t *testing.T) {
	drop := plan.Statement{SQL: "ALTER TABLE t DROP COLUMN legacy", Route: planner.RouteNative,
		Backend: router.BackendNative, Disposition: router.DispositionExecute,
		Drops: &schemadiff.DroppedObject{Kind: schemadiff.DroppedColumn, Name: "legacy", Definition: "attnum 3: text"}}
	base := plan.Fingerprint([]plan.Statement{drop})
	require.NotEqual(t, plan.Fingerprint([]plan.Statement{{SQL: drop.SQL, Route: drop.Route,
		Backend: drop.Backend, Disposition: drop.Disposition}}), base, "a drop with no definition is another plan")

	for name, changed := range map[string]schemadiff.DroppedObject{
		"kind":       {Kind: schemadiff.DroppedNotNull, Name: "legacy", Definition: "attnum 3: text"},
		"name":       {Kind: schemadiff.DroppedColumn, Name: "other", Definition: "attnum 3: text"},
		"definition": {Kind: schemadiff.DroppedColumn, Name: "legacy", Definition: "attnum 4: text"},
		"dependents": {Kind: schemadiff.DroppedColumn, Name: "legacy", Definition: "attnum 3: text", Dependents: []string{"index legacy_idx: CREATE INDEX legacy_idx ON t (legacy)"}},
	} {
		other := drop
		other.Drops = &changed
		assert.NotEqual(t, base, plan.Fingerprint([]plan.Statement{other}), "the dropped object's %s is part of identity", name)
	}
}

// An ADD PRIMARY KEY that runs as the partitioned key build is not the same
// plan as the statement run literally on a plain table.
func TestFingerprintCoversPartitionedKeyBuildExecution(t *testing.T) {
	literal := plan.Statement{
		SQL:         "ALTER TABLE public.t ADD CONSTRAINT t_pkey PRIMARY KEY (id)",
		ExecSQL:     []string{"ALTER TABLE public.t ADD CONSTRAINT t_pkey PRIMARY KEY (id)"},
		Route:       planner.RouteNative,
		Backend:     router.BackendNative,
		Disposition: router.DispositionExecute,
	}
	partitioned := literal
	partitioned.Execution = planner.ExecutionPartitionedKeyBuild

	assert.NotEqual(t,
		plan.Fingerprint([]plan.Statement{literal}),
		plan.Fingerprint([]plan.Statement{partitioned}),
		"the partitioned key build is not the literal ADD CONSTRAINT")
}

// The extension schemas a plan was derived with are part of its identity:
// a table that does not exist yet is created from the file's own SQL, whose
// unqualified names resolve through them. A plan without them hashes as it
// always has, and the serialization with them is pinned like the rest.
func TestFingerprintCoversExtensionSchemas(t *testing.T) {
	st := plan.Statement{SQL: "CREATE TABLE s.t (email citext)", Route: planner.RouteNative,
		Backend: router.BackendNative, Disposition: router.DispositionExecute}
	base := plan.Fingerprint([]plan.Statement{st})
	assert.Equal(t, base, plan.Fingerprint([]plan.Statement{st}, []string{}...),
		"no extension schemas hash exactly as before")

	resolved := plan.Fingerprint([]plan.Statement{st}, "extensions")
	assert.Equal(t, "sha256:bb2fe2be4d00807a969c3240b1bc8105bf5b7ea0fbeba3c615c0d9db7ad85972", resolved)
	assert.NotEqual(t, base, resolved, "the same SQL resolved through an extension schema is a different plan")
	assert.NotEqual(t, resolved, plan.Fingerprint([]plan.Statement{st}, "other"),
		"other extension schemas are a different plan")
	assert.NotEqual(t, plan.Fingerprint([]plan.Statement{st}, "a", "b"), plan.Fingerprint([]plan.Statement{st}, "b", "a"),
		"search order is part of identity")
}

// Sources is the closed vocabulary a consumer branches on; the set is
// pinned to format_version 28.
func TestSourcesVocabularyPinned(t *testing.T) {
	assert.Equal(t, []plan.Source{plan.SourceAlter, plan.SourceDiff}, plan.Sources())
}
