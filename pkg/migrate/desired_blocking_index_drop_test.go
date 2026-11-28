package migrate

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/verdict"
)

// blockingDropReport is a reviewed plan for the partitioned table
// app.events that adds a column and drops the parent's index, which
// PostgreSQL cannot drop concurrently: the drop stays refused and carries
// the plain DROP INDEX with the blocking-index-drop execution.
func blockingDropReport() plan.Report {
	return plan.Report{
		Schema:      "app",
		Table:       "events",
		Fingerprint: "sha256:reviewed",
		Disposition: router.DispositionRefuse,
		Reason:      verdict.ReasonUnsupportedPartitionedParent,
		Class:       verdict.ClassByDesign,
		Statements: []plan.Statement{
			{
				SQL:         `DROP INDEX app.events_kind_idx`,
				Kind:        schemadiff.ChangeDropIndex,
				Destructive: true,
				Disposition: router.DispositionRefuse,
				Reason:      verdict.ReasonUnsupportedPartitionedParent,
				Class:       verdict.ClassByDesign,
				ExecSQL:     []string{`DROP INDEX app.events_kind_idx`},
				Execution:   planner.ExecutionBlockingIndexDrop,
				Drops: &schemadiff.DroppedObject{Kind: schemadiff.DroppedIndex, Name: "events_kind_idx",
					Definition: "CREATE INDEX events_kind_idx ON ONLY app.events USING btree (kind)"},
			},
			{SQL: `ALTER TABLE app.events ADD COLUMN note text`, Disposition: router.DispositionExecute},
		},
	}
}

// The accept-blocking acknowledgement lifts a partitioned table's index drop
// only beside the pin and the destructive acknowledgement, all naming the
// table; without it the plan is refused as it always was, and the refusal
// names the flags that run it.
func TestAdmitPlanAcceptsTheBlockingIndexDrop(t *testing.T) {
	reviewed := DesiredRequest{ExpectedFingerprint: "sha256:reviewed", AllowDestructive: "app.events"}

	t.Run("accepted beside the pin and the destructive acknowledgement", func(t *testing.T) {
		req := reviewed
		req.AcceptBlocking = "app.events"
		_, ok := AdmitPlan(req, blockingDropReport())
		assert.True(t, ok)
	})

	t.Run("without the acceptance the plan is refused and the detail names the flags", func(t *testing.T) {
		res, ok := AdmitPlan(reviewed, blockingDropReport())
		require.False(t, ok)
		assert.Equal(t, verdict.ReasonDestructiveChange, res.Reason)
		assert.Equal(t, verdict.ClassByDesign, res.Class)
		assert.Contains(t, res.Detail, "takes ACCESS EXCLUSIVE on app.events, every partition, and every partition's index")
		assert.Contains(t, res.Detail,
			"--expect-fingerprint sha256:reviewed --allow-destructive app.events --accept-blocking app.events --statement-timeout DURATION")
	})

	t.Run("the acceptance alone lifts nothing", func(t *testing.T) {
		res, ok := AdmitPlan(DesiredRequest{AcceptBlocking: "app.events"}, blockingDropReport())
		require.False(t, ok)
		assert.Equal(t, verdict.ReasonDestructiveChange, res.Reason)
	})

	t.Run("an acceptance naming another table lifts nothing", func(t *testing.T) {
		req := reviewed
		req.AcceptBlocking = "app.orders"
		res, ok := AdmitPlan(req, blockingDropReport())
		require.False(t, ok)
		assert.Equal(t, verdict.ReasonDestructiveChange, res.Reason)
	})

	t.Run("a partitioned index drop without the blocking execution stays refused", func(t *testing.T) {
		report := blockingDropReport()
		report.Statements[0].ExecSQL, report.Statements[0].Execution = nil, ""
		req := reviewed
		req.AcceptBlocking = "app.events"
		res, ok := AdmitPlan(req, report)
		require.False(t, ok)
		assert.Equal(t, verdict.ReasonDestructiveChange, res.Reason)
		assert.Contains(t, res.Detail, "plain DROP INDEX through the imperative front door")
	})

	t.Run("any other refused statement still refuses the plan", func(t *testing.T) {
		report := blockingDropReport()
		report.Statements = append(report.Statements, plan.Statement{
			SQL:         `ALTER TABLE app.events ADD CONSTRAINT events_user_fk FOREIGN KEY (user_id) REFERENCES app.users (id)`,
			Disposition: router.DispositionRefuse,
			Reason:      verdict.ReasonDeclarativeForeignKey,
			Class:       verdict.ClassCapabilityBoundary,
		})
		req := reviewed
		req.AcceptBlocking = "app.events"
		res, ok := AdmitPlan(req, report)
		require.False(t, ok)
		assert.Equal(t, verdict.ReasonDeclarativeForeignKey, res.Reason)
	})
}

// The acceptance is checked before any database work: it needs the pin and
// the destructive acknowledgement, and a brief budget the blocking drop can
// run under, so a run that would refuse the drop stops before any planned
// statement commits. The pool is nil: reaching it would panic.
func TestRunDesiredChecksTheAcceptanceBeforeAnyDatabaseWork(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  DesiredRequest
		want error
	}{
		{
			// The destructive acknowledgement's own check comes first.
			name: "without the pin",
			req:  DesiredRequest{Schema: "app", AllowDestructive: "app.events", AcceptBlocking: "app.events"},
			want: ErrAllowDestructiveNeedsFingerprint,
		},
		{
			name: "without the destructive acknowledgement",
			req:  DesiredRequest{Schema: "app", ExpectedFingerprint: "sha256:r", AcceptBlocking: "app.events"},
			want: ErrAcceptBlockingNeedsReviewedDrop,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := RunDesired(t.Context(), nil, tc.req, DefaultOptions())
			require.ErrorIs(t, err, tc.want)
			assert.Equal(t, DesiredResult{}, res)
		})
	}

	t.Run("a statement timeout not longer than the lock timeout", func(t *testing.T) {
		opts := DefaultOptions()
		opts.Budget.Brief = executor.Budget{LockTimeout: 3 * time.Second, StatementTimeout: 3 * time.Second}
		res, err := RunDesired(t.Context(), nil, DesiredRequest{
			Schema: "app", ExpectedFingerprint: "sha256:r", AllowDestructive: "app.events", AcceptBlocking: "app.events",
		}, opts)
		require.ErrorIs(t, err, executor.ErrInvalidBlockingBudget)
		assert.Equal(t, DesiredResult{}, res)
	})
}

// blockingIndexDropFailureDetail renders what a failed blocking drop left
// behind: drift dropped nothing and needs a re-plan, a commit the client
// could not confirm may have landed and must be inspected before a retry,
// and every other failure rolled back with the index standing.
func TestBlockingIndexDropFailureDetailSaysWhatTheFailureLeftBehind(t *testing.T) {
	drift := blockingIndexDropFailureDetail(fmt.Errorf("check: %w", executor.ErrDroppedIndexDrift), "app.events", "events_kind_idx")
	assert.Contains(t, drift, "nothing was dropped on app.events")
	assert.Contains(t, drift, "re-plan")

	unknown := blockingIndexDropFailureDetail(&executor.BlockingOutcomeUnknownError{Err: errors.New("connection reset")}, "app.events", "events_kind_idx")
	assert.Contains(t, unknown, "outcome unknown")
	assert.Contains(t, unknown, "DROP INDEX events_kind_idx")
	assert.Contains(t, unknown, "inspect the indexes of app.events before retrying")
	assert.NotContains(t, unknown, "rolled back")

	other := blockingIndexDropFailureDetail(errors.New("connection refused"), "app.events", "events_kind_idx")
	assert.Contains(t, other, "index events_kind_idx did not commit and was rolled back")
	assert.Contains(t, other, "still stands on app.events and every partition")
}

// A refused foreign-key drop says which kind of drop it is, and names the
// one remedy that applies: a key changed under the same name is declared
// under a new name, and a key a desired file cannot declare is removed
// through the imperative front door. Without a cause, it names both.
func TestStatementRefusalNamesTheForeignKeyDropsRemedy(t *testing.T) {
	refused := func(cause verdict.Cause) plan.Statement {
		return plan.Statement{
			SQL:          `ALTER TABLE app.events DROP CONSTRAINT events_user_fk`,
			Disposition:  router.DispositionRefuse,
			Reason:       verdict.ReasonDeclarativeForeignKey,
			Class:        verdict.ClassCapabilityBoundary,
			RefusalCause: cause,
		}
	}
	for name, tc := range map[string]struct {
		cause   verdict.Cause
		remedy  string
		without string
	}{
		"a key changed under the same name": {cause: verdict.CauseChangedForeignKey, remedy: "declare the changed key under a new name", without: "migrate --alter"},
		"a key a file cannot declare":       {cause: verdict.CauseUndeclarableForeignKey, remedy: "migrate --alter", without: "under a new name"},
	} {
		r, detail := statementRefusal(0, refused(tc.cause))
		assert.Equal(t, tc.cause, r.Cause(), name)
		assert.Contains(t, detail, tc.remedy, name)
		assert.NotContains(t, detail, tc.without, name)
	}

	r, detail := statementRefusal(0, refused(""))
	assert.Equal(t, verdict.ReasonDeclarativeForeignKey, r.Reason())
	assert.Contains(t, detail, "under a new name")
	assert.Contains(t, detail, "migrate --alter")
}
