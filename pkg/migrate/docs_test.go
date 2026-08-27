package migrate

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/planner"
)

// optimisticAttemptDoc documents the size-guard rule per planner reason;
// these tests keep its per-reason table honest the same way
// pkg/verdict/docs_test.go keeps docs/cli-output-examples.md honest.
const optimisticAttemptDoc = "../../docs/optimistic-attempt.md"

// Every planner reason must have a row in the doc's per-reason size-guard
// table: a Reason constant added without a row fails here.
func TestOptimisticAttemptDocListsEveryPlannerReason(t *testing.T) {
	raw, err := os.ReadFile(optimisticAttemptDoc)
	require.NoError(t, err)
	doc := string(raw)
	for _, r := range planner.Reasons() {
		assert.Contains(t, doc, fmt.Sprintf("| `%s` |", string(r)),
			"docs/optimistic-attempt.md is missing a size-guard row for planner reason %q", r)
	}
}

// TestSizeGuardAppliesPerReason pins the semantics the doc's per-reason
// table describes: the guard covers exactly the blind attempt of the
// submitted form. Reasons that route to copy-and-swap or refuse never
// reach sizeGuardApplies, so only the executing shapes appear here.
func TestSizeGuardAppliesPerReason(t *testing.T) {
	singleReason := func(r planner.Reason) planner.Plan {
		return planner.Plan{Decisions: []planner.Decision{{Reason: r}}}
	}

	// Guarded: the submitted form runs as one blind bounded attempt,
	// however confidently it was classified.
	for _, r := range []planner.Reason{
		planner.ReasonMetadataOnly,
		planner.ReasonFastDefault,
		planner.ReasonBinaryCoercible,
		planner.ReasonAppBreakingRename,
		planner.ReasonPartitionParentLock,
	} {
		assert.True(t, sizeGuardApplies(singleReason(r), false),
			"reason %q executes the submitted form blind and must be size-guarded", r)
	}

	// Exempt: the submitted form is already the safe online idiom.
	assert.False(t, sizeGuardApplies(singleReason(planner.ReasonOnlineIdiom), false),
		"an online-idiom plan is exempt: long work on a large table is its purpose")

	// Exempt: a safer-idiom decision executes only via the planner's
	// substituted sequence, and substitution lifts the guard.
	assert.True(t, sizeGuardApplies(singleReason(planner.ReasonSaferIdiom), false),
		"an unsubstituted plan is guarded regardless of reason")
	assert.False(t, sizeGuardApplies(singleReason(planner.ReasonSaferIdiom), true),
		"the substituted safer sequence is exempt: the sequence is the proof")

	// The exemption requires *every* decision to be an online idiom: one
	// blind statement in a mixed plan keeps the whole run guarded.
	mixed := planner.Plan{Decisions: []planner.Decision{
		{Reason: planner.ReasonOnlineIdiom},
		{Reason: planner.ReasonMetadataOnly},
	}}
	assert.True(t, sizeGuardApplies(mixed, false),
		"a mixed plan containing any blind attempt stays size-guarded")
}
