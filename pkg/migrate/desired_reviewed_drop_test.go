package migrate

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// destructiveReport is a reviewed plan that adds a column and drops another,
// every statement executable, with the fingerprint a reviewer pinned.
func destructiveReport() plan.Report {
	return plan.Report{
		Schema:      "app",
		Table:       "orders",
		Fingerprint: "sha256:reviewed",
		Disposition: router.DispositionExecute,
		Statements: []plan.Statement{
			{SQL: `ALTER TABLE app.orders ADD COLUMN note text`, Disposition: router.DispositionExecute},
			{SQL: `ALTER TABLE app.orders DROP COLUMN legacy_id`, Destructive: true, Disposition: router.DispositionExecute},
		},
	}
}

// A destructive plan is admitted only when the request names the desired
// table and pins the plan; each of the three missing, wrong, or stale
// pieces refuses the plan whole, and the refusal names the way through.
func TestAdmitPlanDestructiveAcknowledgement(t *testing.T) {
	report := destructiveReport()

	t.Run("a pinned plan with its table named is admitted", func(t *testing.T) {
		_, ok := admitPlan(DesiredRequest{AllowDestructive: "app.orders", ExpectedFingerprint: "sha256:reviewed"}, report)
		assert.True(t, ok)
	})

	t.Run("no acknowledgement refuses and names the way through", func(t *testing.T) {
		res, ok := admitPlan(DesiredRequest{ExpectedFingerprint: "sha256:reviewed"}, report)
		require.False(t, ok)
		assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
		assert.Equal(t, verdict.ReasonDestructiveChange, res.Reason)
		assert.Contains(t, res.Detail, "planned statement 2 discards live structure (ALTER TABLE app.orders DROP COLUMN legacy_id)")
		assert.Contains(t, res.Detail, "ExpectedFingerprint sha256:reviewed")
		assert.Contains(t, res.Detail, "AllowDestructive app.orders")
		assert.Contains(t, res.Detail, "the plan's other statement, even if non-destructive, was not run")
	})

	t.Run("a pinned drop beside a copy-and-swap statement is refused", func(t *testing.T) {
		withCopySwap := destructiveReport()
		withCopySwap.Statements = append(withCopySwap.Statements, plan.Statement{
			SQL:         "ALTER TABLE app.orders ALTER COLUMN total TYPE int",
			Disposition: router.DispositionExecute,
			Backend:     router.BackendCopyAndSwap,
		})
		res, ok := admitPlan(DesiredRequest{AllowDestructive: "app.orders", ExpectedFingerprint: "sha256:reviewed"}, withCopySwap)
		require.False(t, ok)
		assert.Equal(t, verdict.ReasonDestructiveChange, res.Reason)
		assert.Contains(t, res.Detail, "planned statement 2 discards live structure (ALTER TABLE app.orders DROP COLUMN legacy_id)")
		assert.Contains(t, res.Detail, "planned statement 3 (ALTER TABLE app.orders ALTER COLUMN total TYPE int) routes to copy-and-swap")
	})

	t.Run("an acknowledgement without a pin is not admitted", func(t *testing.T) {
		res, ok := admitPlan(DesiredRequest{AllowDestructive: "app.orders"}, report)
		require.False(t, ok)
		assert.Equal(t, verdict.ReasonDestructiveChange, res.Reason)
	})

	t.Run("an acknowledgement naming another table is not admitted", func(t *testing.T) {
		res, ok := admitPlan(DesiredRequest{AllowDestructive: "app.other", ExpectedFingerprint: "sha256:reviewed"}, report)
		require.False(t, ok)
		assert.Equal(t, verdict.ReasonDestructiveChange, res.Reason)
	})

	t.Run("a stale pin refuses before the acknowledgement is read", func(t *testing.T) {
		res, ok := admitPlan(DesiredRequest{AllowDestructive: "app.orders", ExpectedFingerprint: "sha256:older"}, report)
		require.False(t, ok)
		assert.Equal(t, verdict.ReasonPlanFingerprintMismatch, res.Reason)
		assert.Contains(t, res.Detail, "sha256:older")
		assert.Contains(t, res.Detail, "sha256:reviewed")
	})

	t.Run("an admitted destructive plan is still refused for a statement that cannot run", func(t *testing.T) {
		blocked := destructiveReport()
		blocked.Disposition = router.DispositionRewriteRequired
		blocked.Statements[0].Disposition = router.DispositionRewriteRequired
		res, ok := admitPlan(DesiredRequest{AllowDestructive: "app.orders", ExpectedFingerprint: "sha256:reviewed"}, blocked)
		require.False(t, ok)
		assert.Equal(t, verdict.ReasonRewriteRequired, res.Reason)
	})
}

// The acknowledgement must name the plan's table whether or not the plan
// drops anything, so one left on a script cannot reach another table.
func TestCheckDesiredAllowDestructiveTable(t *testing.T) {
	report := destructiveReport()
	require.NoError(t, CheckDesiredAllowDestructiveTable("", report))
	require.NoError(t, CheckDesiredAllowDestructiveTable("app.orders", report))

	err := CheckDesiredAllowDestructiveTable("app.other", report)
	require.ErrorIs(t, err, ErrAllowDestructiveMismatch)
	mismatch, ok := errors.AsType[*AcknowledgementMismatchError](err)
	require.True(t, ok)
	assert.Equal(t, "app.orders", mismatch.Table)
	assert.Equal(t, "app.other", mismatch.Given)
	assert.Equal(t, "allow-destructive", mismatch.Acknowledgement)
}

// A destructive statement that reaches execution without an admitted
// acknowledgement, or without the definition of what it drops, is a breach of
// this package's own contract, and nothing about it runs: the nil pool proves
// no database is touched. A statement without a definition cannot be
// re-checked before it runs, so it is refused even for an acknowledged plan.
func TestExecutePlanRunsNoDestructiveStatementItCannotRecheck(t *testing.T) {
	cases := []struct {
		name        string
		acknowledge string
		want        string
	}{
		{"no acknowledgement", "", "planned statement 1 of 1 stopped before a verdict"},
		{"no definition of what it drops", "app.orders", "planned statement 1 of 1 stopped before a verdict"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := destructiveReport()
			report.Statements = report.Statements[1:]
			res, err := executePlan(t.Context(), nil, report, DefaultOptions(), tc.acknowledge, nil)
			require.ErrorIs(t, err, executor.ErrInvariantViolation)
			assert.Equal(t, verdict.OutcomeFailed, res.Outcome)
			assert.Empty(t, res.Verdicts)
			assert.Contains(t, res.Detail, tc.want)
		})
	}
}

// A planned index drop runs only in the shape the planner promised: one
// index of the plan's own schema, executing as exactly one DROP INDEX
// CONCURRENTLY of that same index.
func TestPlannedIndexDropMustExecuteConcurrently(t *testing.T) {
	report := plan.Report{Schema: "app", Table: "orders"}
	drop := func(sql string, exec ...string) plan.Statement {
		return plan.Statement{SQL: sql, Kind: schemadiff.ChangeDropIndex, Destructive: true, ExecSQL: exec,
			Drops: &schemadiff.DroppedObject{Kind: schemadiff.DroppedIndex, Name: "orders_qty_idx", Definition: "CREATE INDEX orders_qty_idx ON orders USING btree (qty)"}}
	}
	const sql = `DROP INDEX app.orders_qty_idx`
	const concurrent = `DROP INDEX CONCURRENTLY app.orders_qty_idx`

	withDrops := func(ps plan.Statement, d *schemadiff.DroppedObject) plan.Statement {
		ps.Drops = d
		return ps
	}
	parse := func(t *testing.T, sql string) statement.Statement {
		t.Helper()
		st, err := statement.ParseOne(sql)
		require.NoError(t, err)
		return st
	}

	t.Run("the concurrent form of the same index is accepted", func(t *testing.T) {
		index, definition, err := plannedIndexDrop(report, drop(sql, concurrent), parse(t, sql))
		require.NoError(t, err)
		assert.Equal(t, "orders_qty_idx", index)
		assert.Equal(t, "CREATE INDEX orders_qty_idx ON orders USING btree (qty)", definition)
	})

	refused := []struct {
		name string
		ps   plan.Statement
		sql  string
	}{
		{"a blocking execution form", drop(sql, sql), sql},
		{"no execution form", drop(sql), sql},
		{"two execution forms", drop(sql, concurrent, concurrent), sql},
		{"another index", drop(sql, `DROP INDEX CONCURRENTLY app.orders_other_idx`), sql},
		{"another schema", drop(sql, `DROP INDEX CONCURRENTLY other.orders_qty_idx`), sql},
		{"a cascading drop", drop(sql, `DROP INDEX CONCURRENTLY app.orders_qty_idx CASCADE`), sql},
		{"a statement in another schema", drop(`DROP INDEX other.orders_qty_idx`, concurrent), `DROP INDEX other.orders_qty_idx`},
		{"an unqualified statement", drop(`DROP INDEX orders_qty_idx`, concurrent), `DROP INDEX orders_qty_idx`},
		{"not an index drop", drop(`ALTER TABLE app.orders DROP COLUMN note`, concurrent), `ALTER TABLE app.orders DROP COLUMN note`},
		{"no definition of the index", withDrops(drop(sql, concurrent), nil), sql},
		{"an empty definition", withDrops(drop(sql, concurrent), &schemadiff.DroppedObject{Kind: schemadiff.DroppedIndex, Name: "orders_qty_idx"}), sql},
		{"a definition of another index", withDrops(drop(sql, concurrent), &schemadiff.DroppedObject{Kind: schemadiff.DroppedIndex, Name: "orders_other_idx", Definition: "CREATE INDEX x"}), sql},
		{"a definition of another kind of object", withDrops(drop(sql, concurrent), &schemadiff.DroppedObject{Kind: schemadiff.DroppedConstraint, Name: "orders_qty_idx", Definition: "CHECK (qty > 0)"}), sql},
	}
	for _, tc := range refused {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			_, _, err := plannedIndexDrop(report, tc.ps, parse(t, tc.sql))
			require.ErrorIs(t, err, executor.ErrInvariantViolation)
		})
	}
}
