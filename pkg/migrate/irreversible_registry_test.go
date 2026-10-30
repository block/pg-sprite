package migrate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// withAck returns opts carrying value as acknowledgement a. It is written
// independently of ackKind.value, so a value that reads the wrong Options
// field fails the round trip below.
func withAck(t *testing.T, opts Options, a ackKind, value string) Options {
	t.Helper()
	switch a {
	case ackForce:
		opts.Force = value
	case ackAcceptBlocking:
		opts.AcceptBlocking = value
	case ackAllowDestructive:
		opts.AllowDestructive = value
	case ackRestart:
		opts.CopySwap.Restart = value
	case ackBlockWrites:
		opts.CopySwap.BlockWrites = value
	default:
		t.Fatalf("acknowledgement %q has no Options field in this test", a)
	}
	return opts
}

// The registry lists every irreversible operation with the acknowledgement
// that lifts its refusal, and desired-state execution takes none of those
// acknowledgements. A new acknowledgement, or a new shape the planner marks
// destructive, fails here until it has an entry and a desired-state
// decision.
func TestIrreversibleOperationRegistryIsComplete(t *testing.T) {
	acks := ackKinds()
	ops := irreversibleOps()
	destructive := planner.DestructiveOpKinds()

	t.Run("the walk is not vacuous", func(t *testing.T) {
		require.NotEmpty(t, destructive)
		require.Contains(t, destructive, statement.OpDropColumn)
		assert.Len(t, acks, 5)
		keys := make([]string, 0, len(ops))
		for _, op := range ops {
			keys = append(keys, op.key)
		}
		assert.Contains(t, keys, "copy-and-swap restart")
		assert.Len(t, ops, len(destructive)+5)
	})

	t.Run("every acknowledgement reads its own field and has its own sentinel", func(t *testing.T) {
		sentinels := map[error]ackKind{}
		for _, a := range acks {
			got, ok := a.value(withAck(t, DefaultOptions(), a, "app.orders"))
			require.True(t, ok, "acknowledgement %q is not classified", a)
			assert.Equal(t, "app.orders", got, "acknowledgement %q reads another field", a)
			for _, other := range acks {
				if other != a {
					v, _ := other.value(withAck(t, DefaultOptions(), a, "app.orders"))
					assert.Empty(t, v, "acknowledgement %q also reads %q's field", other, a)
				}
			}
			err := a.desiredErr()
			require.Error(t, err, "acknowledgement %q has no desired-state sentinel", a)
			prev, dup := sentinels[err]
			assert.False(t, dup, "acknowledgements %q and %q share a sentinel", prev, a)
			sentinels[err] = a
		}
	})

	// Each acknowledgement accepts a loss for one statement the operator
	// wrote; desired-state execution runs statements the plan derived, so
	// it rejects every one before any database work, even when it names
	// the right table.
	t.Run("desired-state execution rejects every acknowledgement", func(t *testing.T) {
		for _, a := range acks {
			opts := withAck(t, DefaultOptions(), a, "public.t")
			res, err := RunDesired(t.Context(), nil, DesiredRequest{Schema: "public"}, opts)
			require.ErrorIs(t, err, a.desiredErr(), "acknowledgement %q", a)
			assert.Equal(t, DesiredResult{}, res)
		}
	})

	t.Run("every destructive shape has exactly one entry", func(t *testing.T) {
		var shapes []string
		for _, op := range ops {
			if op.desired == desiredRefusesPlan {
				shapes = append(shapes, op.key)
			}
		}
		want := make([]string, 0, len(destructive))
		for _, k := range destructive {
			want = append(want, "shape: "+statement.Op{Kind: k}.Describe())
		}
		assert.Equal(t, want, shapes)
	})

	t.Run("every entry is gated and every acknowledgement gates an entry", func(t *testing.T) {
		gated := map[ackKind]bool{}
		for _, op := range ops {
			assert.Contains(t, acks, op.ack, "entry %q", op.key)
			gated[op.ack] = true
			assert.Contains(t, []loss{lossStructure, lossAvailability, lossProgress}, op.loss, "entry %q", op.key)
			assert.Contains(t, []desiredHandling{desiredRefusesPlan, desiredRejectsAck}, op.desired, "entry %q", op.key)
			if op.loss == lossProgress {
				assert.Empty(t, op.refusal, "entry %q: without the acknowledgement the run resumes", op.key)
				continue
			}
			assert.Contains(t, verdict.Reasons(), op.refusal, "entry %q", op.key)
		}
		for _, a := range acks {
			assert.True(t, gated[a], "acknowledgement %q gates no entry", a)
		}
	})

	// The shapes desired-state execution can derive are refused at plan
	// admission, whole, before any statement runs.
	t.Run("desired-state admission refuses a destructive plan whole", func(t *testing.T) {
		report := plan.Report{
			Schema: "app",
			Table:  "orders",
			Statements: []plan.Statement{
				{SQL: `ALTER TABLE app.orders ADD COLUMN note text`},
				{SQL: `ALTER TABLE app.orders DROP COLUMN legacy_id`, Destructive: true},
			},
		}
		res, ok := admitPlan(DesiredRequest{}, report)
		require.False(t, ok)
		assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
		assert.Equal(t, verdict.ReasonDestructiveChange, res.Reason)
		for _, op := range ops {
			if op.desired == desiredRefusesPlan {
				assert.Equal(t, verdict.ReasonDestructiveChange, op.refusal, "entry %q", op.key)
			}
		}
	})
}
