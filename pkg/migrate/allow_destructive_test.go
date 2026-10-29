package migrate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/suggest"
	"github.com/block/pg-sprite/pkg/verdict"
)

// routeOne classifies and routes sql without live facts, the way a dry run
// against a table it could not introspect would.
func routeOne(t *testing.T, sql string) (statement.Statement, router.Statement) {
	t.Helper()
	st, err := statement.ParseOne(sql)
	require.NoError(t, err)
	canonical, err := statement.Canonical(st.SQL())
	require.NoError(t, err)
	classified, err := planner.Classify(canonical, planner.Facts{})
	require.NoError(t, err)
	return st, router.Route([]planner.Plan{classified}).Statements[0]
}

// The destructive acknowledgement and accepted blocking are separate
// policy paths; both at once is rejected before any database work.
func TestRunRejectsAllowDestructiveWithAcceptBlocking(t *testing.T) {
	st, err := statement.ParseOne("DROP INDEX app.orders_created_idx")
	require.NoError(t, err)
	opts := DefaultOptions()
	opts.AllowDestructive = "app.orders"
	opts.AcceptBlocking = "app.orders"

	v, err := Run(t.Context(), nil, st, opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AllowDestructive")
	assert.Equal(t, verdict.Verdict{}, v)
}

// Desired-state execution refuses a destructive plan whole, so the
// acknowledgement has nothing to acknowledge there and is rejected as a
// typed sentinel before any database work.
func TestRunDesiredRejectsAllowDestructive(t *testing.T) {
	opts := DefaultOptions()
	opts.AllowDestructive = "public.t"

	res, err := RunDesired(t.Context(), nil, DesiredRequest{}, opts)
	require.ErrorIs(t, err, ErrAllowDestructiveNotSupported)
	assert.Equal(t, DesiredResult{}, res)
}

// The dry run's gate decides a DROP COLUMN exactly as Run does: refused
// without the acknowledgement, admitted with one naming the table, and a
// usage error with one naming another table.
func TestAdmitDestructive(t *testing.T) {
	st, rs := routeOne(t, "ALTER TABLE app.orders DROP COLUMN note")

	t.Run("refuses without the acknowledgement", func(t *testing.T) {
		v, refused, err := AdmitDestructive(st, "app", rs, "")
		require.NoError(t, err)
		require.True(t, refused)
		assert.Equal(t, verdict.ReasonDestructiveChange, v.Reason)
		assert.Equal(t, verdict.ClassByDesign, v.Class)
		assert.Equal(t, "app.orders", v.Table)
	})

	t.Run("admits the acknowledged statement", func(t *testing.T) {
		_, refused, err := AdmitDestructive(st, "app", rs, "app.orders")
		require.NoError(t, err)
		assert.False(t, refused)
	})

	t.Run("rejects an acknowledgement naming another table", func(t *testing.T) {
		_, refused, err := AdmitDestructive(st, "app", rs, "public.orders")
		require.ErrorIs(t, err, ErrAllowDestructiveMismatch)
		assert.False(t, refused)
	})

	t.Run("qualifies an unqualified table with the resolved schema", func(t *testing.T) {
		bare, bareRS := routeOne(t, "ALTER TABLE orders DROP COLUMN note")
		v, refused, err := AdmitDestructive(bare, "app", bareRS, "")
		require.NoError(t, err)
		require.True(t, refused)
		assert.Equal(t, "app.orders", v.Table)
		_, _, err = AdmitDestructive(bare, "app", bareRS, "app.orders")
		require.NoError(t, err)
	})

	// The deparser cannot re-emit every unqualified statement qualified (a
	// rename, for one), so the gate qualifies only a statement it refuses
	// and leaves the rest to the dry run's own report.
	t.Run("leaves an unqualified statement that discards nothing untouched", func(t *testing.T) {
		rename, renameRS := routeOne(t, "ALTER TABLE users RENAME COLUMN name TO full_name")
		_, refused, err := AdmitDestructive(rename, "public", renameRS, "")
		require.NoError(t, err)
		assert.False(t, refused)
	})

	t.Run("admits a statement that discards nothing", func(t *testing.T) {
		add, addRS := routeOne(t, "ALTER TABLE app.orders ADD COLUMN sku text")
		_, refused, err := AdmitDestructive(add, "app", addRS, "")
		require.NoError(t, err)
		assert.False(t, refused)
	})
}

// A planner refusal runs nothing and no acknowledgement lifts it, so the
// destructive gate leaves it alone and it keeps its own reason.
func TestDestructiveGateLeavesPlannerRefusalsAlone(t *testing.T) {
	rs := router.Statement{
		Plan: planner.Plan{
			Decisions: []planner.Decision{{Operation: "DROP COLUMN ref", Destructive: true}},
		},
		Disposition: router.DispositionRefuse,
	}
	assert.Empty(t, destructiveOperations(rs))
}

// Dropping a column and adding it back under the same name carries the
// guidance to change the column in place; a drop that adds nothing back,
// or adds a different column, carries none.
func TestDestructiveChangeVerdictGuidance(t *testing.T) {
	reAdd, err := statement.ParseOne(`
		ALTER TABLE app.orders
			DROP COLUMN qty,
			ADD COLUMN qty bigint`)
	require.NoError(t, err)
	v, err := destructiveChangeVerdict(reAdd, []string{"DROP COLUMN qty"})
	require.NoError(t, err)
	assert.Equal(t, suggest.GuidanceAlterColumnTypeInPlace, v.Guidance)

	other, err := statement.ParseOne(`
		ALTER TABLE app.orders
			DROP COLUMN qty,
			ADD COLUMN quantity bigint`)
	require.NoError(t, err)
	v, err = destructiveChangeVerdict(other, []string{"DROP COLUMN qty"})
	require.NoError(t, err)
	assert.Empty(t, v.Guidance)
}
