package migrate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/verdict"
)

// Copy-and-swap admission reads the database only for a plan that routes a
// statement through the backend: a wholly native plan is admitted without
// a connection at all; a routed plan under a policy that keeps the route
// closed is refused whole, as a capability boundary, before any read; and
// a routed plan whose caller opened the route but never wired its
// connection stops before any read with the sentinel, so a library caller
// can tell its own missing wiring from a fact about the table.
func TestAdmitCopySwap(t *testing.T) {
	t.Run("a native plan is admitted without a read", func(t *testing.T) {
		report := plan.Report{Schema: "billing", Table: "orders", Statements: []plan.Statement{
			{SQL: "ALTER TABLE billing.orders ADD COLUMN note text", Backend: router.BackendNative},
		}}
		stopped, admitted, err := admitCopySwap(t.Context(), nil, report, Options{})
		require.NoError(t, err)
		assert.True(t, admitted)
		assert.Zero(t, stopped)
	})

	t.Run("a routed plan under the closed route is refused whole", func(t *testing.T) {
		report := plan.Report{Schema: "billing", Table: "orders", Statements: []plan.Statement{
			{SQL: "ALTER TABLE billing.orders ADD COLUMN note text", Backend: router.BackendNative},
			{SQL: "ALTER TABLE billing.orders ALTER COLUMN id TYPE bigint", Backend: router.BackendCopyAndSwap},
		}}
		refused, admitted, err := admitCopySwap(t.Context(), nil, report, Options{})
		require.NoError(t, err, "a closed route is a refusal, not a wiring error")
		assert.False(t, admitted)
		assert.Equal(t, verdict.OutcomeRefused, refused.Outcome)
		assert.Equal(t, verdict.ReasonCopyAndSwapRefused, refused.Reason)
		assert.Equal(t, verdict.ClassCapabilityBoundary, refused.Class)
		assert.Empty(t, refused.Verdicts, "no statement was attempted, not even the native one planned first")
		assert.Equal(t, report, refused.Plan)
		assert.Contains(t, refused.Detail, "planned statement 2 (ALTER TABLE billing.orders ALTER COLUMN id TYPE bigint)")
		assert.Contains(t, refused.Detail, "nothing was executed")
	})

	t.Run("a routed plan without the connection stops with the sentinel", func(t *testing.T) {
		report := plan.Report{Schema: "billing", Table: "orders", Statements: []plan.Statement{
			{SQL: "ALTER TABLE billing.orders ADD COLUMN note text", Backend: router.BackendNative},
			{SQL: "ALTER TABLE billing.orders ALTER COLUMN id TYPE bigint", Backend: router.BackendCopyAndSwap},
		}}
		opts := Options{CopySwap: CopySwapOptions{Enabled: true}}
		stopped, admitted, err := admitCopySwap(t.Context(), nil, report, opts)
		require.ErrorIs(t, err, ErrCopySwapConnectionRequired)
		assert.False(t, admitted)
		assert.Equal(t, verdict.OutcomeFailed, stopped.Outcome)
		assert.Empty(t, stopped.Verdicts, "no statement was attempted")
		assert.Equal(t, report, stopped.Plan)
		assert.Contains(t, stopped.Detail, "planned statement 2 of 2")
		assert.Contains(t, stopped.Detail, "nothing was executed")
	})
}
