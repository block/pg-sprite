package migrate

import (
	"errors"
	"fmt"
	"testing"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// classifiedKey is one refusal key the registry must classify, paired with
// what it produced. Keys are derived from production closed sets, never
// from a list this test maintains on its own.
type classifiedKey struct {
	key     string
	refusal verdict.Refusal
	ok      bool
}

// deriveRefusalKeys walks every closed set the registry is keyed on —
// executor.CreateShapeCauses(), preflight.PartitionRefusalCauses(), the two
// admission sentinel sets, statement.Kinds() (with and without CONCURRENTLY),
// and the site walk — and classifies each key through the production
// registry. Admitted statement kinds are returned with ok false and are the
// only keys allowed to be.
func deriveRefusalKeys() (keys []classifiedKey, admitted []string) {
	for _, c := range executor.CreateShapeCauses() {
		r, ok := plan.CreateShapeRefusal(c)
		keys = append(keys, classifiedKey{"create-shape:" + string(c), r, ok})
	}
	for _, c := range preflight.PartitionRefusalCauses() {
		r, ok := plan.PartitionRefusal(c)
		keys = append(keys, classifiedKey{"partition:" + string(c), r, ok})
	}
	for _, err := range admissionSentinels() {
		r, ok := admissionRefusal(fmt.Errorf("wrapped: %w", err))
		keys = append(keys, classifiedKey{"admission:" + err.Error(), r, ok})
	}
	for _, err := range createAdmissionSentinels() {
		r, ok := createAdmissionRefusal(fmt.Errorf("wrapped: %w", err))
		keys = append(keys, classifiedKey{"create-admission:" + err.Error(), r, ok})
	}
	for _, k := range statement.Kinds() {
		for _, concurrent := range []bool{false, true} {
			r, ok := gateRefusal(k, concurrent)
			key := fmt.Sprintf("gate:%s/concurrent=%t", k, concurrent)
			if !ok {
				admitted = append(admitted, key)
				continue
			}
			keys = append(keys, classifiedKey{key, r, ok})
		}
	}
	for _, s := range siteRefusals() {
		keys = append(keys, classifiedKey{"site:" + s.site, s.refusal, true})
	}
	keys = append(keys, classifiedKey{"route", plan.RouteRefusal(), true})
	return keys, admitted
}

// TestRefusalRegistryIsComplete is the RF-7 completeness harness
// (docs/refusal-classes.md): every key the production closed sets name is
// classified with a class in verdict.Classes(), a reason in
// verdict.Reasons(), and an owner exactly when the class is
// no-online-safety-problem; every reason is classified by at least one key;
// and the derivation is pinned against vacuous success.
func TestRefusalRegistryIsComplete(t *testing.T) {
	keys, admitted := deriveRefusalKeys()

	// The deriver must have found the sets: a change to how refusals are
	// constructed cannot make it pass on an empty walk.
	require.NotEmpty(t, executor.CreateShapeCauses())
	require.NotEmpty(t, preflight.PartitionRefusalCauses())
	require.NotEmpty(t, admissionSentinels())
	require.NotEmpty(t, createAdmissionSentinels())
	require.NotEmpty(t, siteRefusals())
	require.Greater(t, len(keys), 20, "the walk covers causes, sentinels, kinds, and sites")
	require.ElementsMatch(t, []string{
		"gate:ALTER TABLE/concurrent=false", "gate:ALTER TABLE/concurrent=true",
		"gate:CREATE INDEX/concurrent=false", "gate:CREATE INDEX/concurrent=true",
	}, admitted, "only the two admitted kinds may leave the gate unclassified")
	for _, sentinel := range []string{
		"create-shape:" + string(executor.CreateShapeIfNotExists),
		"partition:" + string(preflight.PartitionCauseIndexAdoption),
		"admission:" + executor.ErrIfNotExistsUnsupported.Error(),
		"create-admission:" + executor.ErrDuplicateCreateName.Error(),
		"gate:" + statement.KindOther.String() + "/concurrent=false",
		"site:plan-incoherent",
	} {
		assert.True(t, hasKey(keys, sentinel), "sentinel key %q missing from the walk", sentinel)
	}

	classified := map[verdict.Reason]bool{}
	for _, k := range keys {
		t.Run(k.key, func(t *testing.T) {
			require.True(t, k.ok, "unclassified refusal key")
			require.False(t, k.refusal.IsZero(), "zero refusal")
			// Re-validate through the general constructor: class, reason,
			// and owner rule.
			_, err := verdict.NewRefusal(k.refusal.Class(), k.refusal.Reason(), k.refusal.Owner())
			require.NoError(t, err)
			classified[k.refusal.Reason()] = true
		})
	}
	for _, reason := range verdict.Reasons() {
		assert.True(t, classified[reason], "reason %q has no classified key", reason)
	}
}

// The class is a property of the cause, not the reason: the same reason
// classifies differently under different discriminators, and the sentinel
// spelling of a cause agrees with the cause spelling. A registry keyed on
// reason or site alone would fail these.
func TestRefusalRegistryCorrespondence(t *testing.T) {
	class := func(r verdict.Refusal, ok bool) verdict.Class {
		require.True(t, ok)
		return r.Class()
	}
	// unsupported-statement spans four classes.
	assert.Equal(t, verdict.ClassByDesign, class(plan.CreateShapeRefusal(executor.CreateShapeIfNotExists)))
	assert.Equal(t, verdict.ClassCapabilityBoundary, class(plan.CreateShapeRefusal(executor.CreateShapePartitionOf)))
	assert.Equal(t, verdict.ClassInvariantViolation, class(plan.CreateShapeRefusal(executor.CreateShapeMultipleOperations)))
	assert.Equal(t, verdict.ClassNoOnlineSafetyProblem, class(gateRefusal(statement.KindDataChange, false)))
	// unsupported-partitioned-parent spans three.
	assert.Equal(t, verdict.ClassCapabilityBoundary, class(plan.PartitionRefusal(preflight.PartitionCauseConcurrentIndexBuild)))
	assert.Equal(t, verdict.ClassByDesign, class(plan.PartitionRefusal(preflight.PartitionCauseIndexAdoption)))
	assert.Equal(t, verdict.ClassEnvironmental, class(plan.PartitionRefusal(preflight.PartitionCauseNotValidForeignKey)))
	// index-statement: the plain form is refused by design, the concurrent
	// form is the operator's to run.
	assert.Equal(t, verdict.ClassByDesign, class(gateRefusal(statement.KindDropIndex, false)))
	assert.Equal(t, verdict.ClassNoOnlineSafetyProblem, class(gateRefusal(statement.KindReindex, true)))
	// Owners: each no-online-safety-problem kind names who runs it.
	for kind, owner := range map[statement.Kind]verdict.Owner{
		statement.KindDataChange:   verdict.OwnerDataChangeRunner,
		statement.KindProvisioning: verdict.OwnerProvisioning,
		statement.KindCatalogWork:  verdict.OwnerDirectOperator,
		statement.KindCreateTable:  verdict.OwnerDeclarativeFrontDoor,
		statement.KindDropIndex:    verdict.OwnerDirectOperator,
	} {
		r, ok := gateRefusal(kind, kind == statement.KindDropIndex)
		require.True(t, ok, kind)
		assert.Equal(t, owner, r.Owner(), kind)
	}
	r, ok := gateRefusal(statement.KindOther, false)
	require.True(t, ok)
	assert.Equal(t, verdict.ClassCapabilityBoundary, r.Class(), "unnamed grammar is a boundary, not someone else's work")
	assert.Empty(t, r.Owner())

	// Sentinel spellings agree with their cause spellings.
	pairs := createShapeSentinelCauses()
	require.Len(t, pairs, 3)
	for sentinel, cause := range pairs {
		fromSentinel, ok := createAdmissionRefusal(sentinel)
		require.True(t, ok, sentinel)
		fromCause, ok := plan.CreateShapeRefusal(cause)
		require.True(t, ok, cause)
		assert.Equal(t, fromCause, fromSentinel, "%v vs %s", sentinel, cause)
	}
	fromSentinel, ok := admissionRefusal(executor.ErrIfNotExistsUnsupported)
	require.True(t, ok)
	fromCause, _ := plan.CreateShapeRefusal(executor.CreateShapeIfNotExists)
	assert.Equal(t, fromCause, fromSentinel)
}

// Membership and classification are one walk: an error outside the sentinel
// set, or a sentinel wrapped in a step error (execution started), is not an
// admission refusal.
func TestAdmissionRefusalMembership(t *testing.T) {
	_, ok := admissionRefusal(errors.New("connection reset"))
	assert.False(t, ok)
	_, ok = admissionRefusal(nil)
	assert.False(t, ok)
	stepErr := &executor.SequenceStepError{Step: 2, Total: 3, Err: executor.ErrUnsupportedSequenceStep}
	_, ok = admissionRefusal(stepErr)
	assert.False(t, ok, "a step error means execution started")
	_, ok = createAdmissionRefusal(executor.ErrCreateCollision)
	assert.False(t, ok, "a collision is not a shape admission refusal")
	_, ok = createAdmissionRefusal(executor.ErrUnnamedIndex)
	assert.False(t, ok, "an imperative sentinel is not a create-path sentinel")
}

func hasKey(keys []classifiedKey, key string) bool {
	for _, k := range keys {
		if k.key == key {
			return true
		}
	}
	return false
}
