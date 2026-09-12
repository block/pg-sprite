// White-box tests for the fail-closed decision helpers of the concurrent
// index build: the pieces whose safety branches (unknown backend states, a
// replaced target table) cannot be reached deterministically through the
// public API against a healthy database.

package executor

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClassifyBackendState(t *testing.T) {
	tests := []struct {
		name  string
		state *string
		want  backendVerdict
	}{
		{name: "idle is stopped", state: new("idle"), want: backendStopped},
		{name: "idle in transaction is stopped", state: new("idle in transaction"), want: backendStopped},
		{name: "idle in aborted transaction is stopped", state: new("idle in transaction (aborted)"), want: backendStopped},
		{name: "active keeps polling", state: new("active"), want: backendRunning},
		{name: "fastpath function call keeps polling", state: new("fastpath function call"), want: backendRunning},
		{name: "NULL state is unprovable: the backend is hidden", state: nil, want: backendUnprovable},
		{name: "disabled is unprovable: track_activities is off", state: new("disabled"), want: backendUnprovable},
		{name: "an unknown state is unprovable", state: new("hibernating"), want: backendUnprovable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, classifyBackendState(tt.state))
		})
	}
}

// TestAsConcurrentBudgetError covers the cancellation partition, in
// precedence order. A server 57014 at or past the overall budget is the
// budget's own statement_timeout whatever the caller's context did. Below
// that, the caller's own context ending is the caller's cancellation in
// either mode and in either form it arrives. Under a live context SQLSTATE
// 57014 is query_canceled generally, so an early one is an external
// cancellation and must not read as budget exhaustion, because a consumer
// branching on *BudgetError escalates to a heavier strategy, the wrong
// reaction to a deliberate operator cancel.
