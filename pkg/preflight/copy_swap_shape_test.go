package preflight

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/refusal"
)

// supportedCopySwapShape is the fact set a plain bigint-keyed table
// reports: every refusal below is one fact away from it.
func supportedCopySwapShape() copySwapShapeFacts {
	return copySwapShapeFacts{
		relkind:         "r",
		relpersistence:  "p",
		replicaIdentity: "d",
		pkColumns:       1,
		pkColumn:        "id",
		pkType:          "bigint",
	}
}

// The cause strings are a published contract, so each enumerated cause must
// be one of the two decisions can actually produce: a cause that is
// declared, enumerated, and documented but never raised would pass the
// other completeness checks. Each shape case flips one fact of the
// supported shape; the environment causes are raised in
// copy_swap_environment_test.go and only accounted for here.
func TestRefuseCopySwapShapeRaisesEveryEnumeratedCause(t *testing.T) {
	raisedBy := map[CopySwapRefusalCause]func(f *copySwapShapeFacts){
		CopySwapCauseUnlogged:           func(f *copySwapShapeFacts) { f.relpersistence = "u" },
		CopySwapCauseForceRLS:           func(f *copySwapShapeFacts) { f.forceRLS = true },
		CopySwapCausePartitioned:        func(f *copySwapShapeFacts) { f.relkind = "p" },
		CopySwapCausePKUnsupported:      func(f *copySwapShapeFacts) { f.pkType = "uuid" },
		CopySwapCauseReplicaIdentity:    func(f *copySwapShapeFacts) { f.replicaIdentity = "n" },
		CopySwapCauseForeignKeys:        func(f *copySwapShapeFacts) { f.foreignKeysIn = 1 },
		CopySwapCauseTriggers:           func(f *copySwapShapeFacts) { f.rules = 1 },
		CopySwapCauseDependentViews:     func(f *copySwapShapeFacts) { f.dependentViews = 1 },
		CopySwapCausePublicationMember:  func(f *copySwapShapeFacts) { f.publications = []string{"analytics"} },
		CopySwapCauseSubscriptionTarget: func(f *copySwapShapeFacts) { f.subscriptions = 1 },
		CopySwapCauseDependents: func(f *copySwapShapeFacts) {
			f.dependents = []string{"rule audit_rows on table audit"}
		},
	}
	for _, cause := range CopySwapRefusalCauses() {
		if _, environment := environmentCauseRaisedBy[cause]; environment {
			continue
		}
		flip, ok := raisedBy[cause]
		require.True(t, ok, "cause %s has no fact that raises it", cause)
		facts := supportedCopySwapShape()
		flip(&facts)
		got, detail := refuseCopySwapShape(facts)
		assert.Equal(t, cause, got)
		assert.NotEmpty(t, detail)
	}
	assert.Len(t, raisedBy, len(CopySwapRefusalCauses())-len(environmentCauseRaisedBy),
		"every case here must be an enumerated cause that is not an environment cause")

	cause, detail := refuseCopySwapShape(supportedCopySwapShape())
	assert.Empty(t, cause)
	assert.Empty(t, detail)
}

// A DEFERRABLE key is refused under the key cause even when its arity and
// type are supported, and the arity and type decide before deferrability,
// since each of those needs a larger change to the table.
func TestRefuseCopySwapShapeRefusesDeferrableKey(t *testing.T) {
	facts := supportedCopySwapShape()
	facts.pkDeferrable = true
	cause, detail := refuseCopySwapShape(facts)
	assert.Equal(t, CopySwapCausePKUnsupported, cause)
	assert.Contains(t, detail, "DEFERRABLE")

	facts.pkType = "uuid"
	_, detail = refuseCopySwapShape(facts)
	assert.Contains(t, detail, "uuid")
}

// Dependents are decided after the table's own facts, and the named
// dependent kinds before the generic one: a table with a trigger and a
// dependent view reports the trigger; one that is both viewed and
// published reports the view; one that is published and subscribed
// reports the publication; and one that is subscribed and has a rule on
// another table writing into it reports the subscription.
func TestRefuseCopySwapShapeDecidesDependentsLast(t *testing.T) {
	facts := supportedCopySwapShape()
	facts.triggers = 1
	facts.dependentViews = 1
	cause, _ := refuseCopySwapShape(facts)
	assert.Equal(t, CopySwapCauseTriggers, cause)

	facts = supportedCopySwapShape()
	facts.dependentViews = 1
	facts.publications = []string{"analytics"}
	cause, _ = refuseCopySwapShape(facts)
	assert.Equal(t, CopySwapCauseDependentViews, cause)

	facts = supportedCopySwapShape()
	facts.publications = []string{"analytics"}
	facts.subscriptions = 1
	cause, _ = refuseCopySwapShape(facts)
	assert.Equal(t, CopySwapCausePublicationMember, cause)

	facts = supportedCopySwapShape()
	facts.subscriptions = 1
	facts.dependents = []string{"rule audit_rows on table audit"}
	cause, _ = refuseCopySwapShape(facts)
	assert.Equal(t, CopySwapCauseSubscriptionTarget, cause)
}

// The engine's own publication for the table is set aside by its exact
// derived name: a publication that merely wears the engine prefix, or that
// is the engine's publication for a different table, still publishes this
// table from outside the route and is reported.
func TestPublicationsOtherThanEngineKeepsEveryNameButTheExactOne(t *testing.T) {
	engine := CopySwapDecodingName("app", "public", "orders")
	otherTable := CopySwapDecodingName("app", "public", "customers")
	assert.Equal(t,
		[]string{"analytics", otherTable, "pgsprite_0badf00d"},
		publicationsOtherThanEngine([]string{"analytics", engine, otherTable, "pgsprite_0badf00d"}, engine))
	assert.Empty(t, publicationsOtherThanEngine([]string{engine}, engine))
	assert.Empty(t, publicationsOtherThanEngine(nil, engine))
}

// Whole-table facts decide before key and dependent facts: a table that is
// both UNLOGGED and keyless reports the durability cause, and a forced-RLS
// table with a foreign key reports the security cause.
func TestRefuseCopySwapShapeDecidesWholeTableFactsFirst(t *testing.T) {
	facts := supportedCopySwapShape()
	facts.relpersistence = "u"
	facts.pkColumns = 0
	cause, _ := refuseCopySwapShape(facts)
	assert.Equal(t, CopySwapCauseUnlogged, cause)

	facts = supportedCopySwapShape()
	facts.forceRLS = true
	facts.foreignKeysOut = 1
	cause, _ = refuseCopySwapShape(facts)
	assert.Equal(t, CopySwapCauseForceRLS, cause)
}

// Every copy-and-swap cause routes to the class docs/refusal-classes.md
// decides for it: a replica identity the operator can set and every cluster
// and volume cause are environmental, since the same table is admitted once
// state outside the engine changes; every other shape cause is a capability
// boundary, since admitting it takes an engine capability; every answer is a
// member of refusal.Classes(), and the zero cause — the shape is supported —
// has no class.
func TestEveryCopySwapRefusalCauseNamesItsClass(t *testing.T) {
	want := map[CopySwapRefusalCause]refusal.Class{
		CopySwapCausePKUnsupported:              refusal.ClassCapabilityBoundary,
		CopySwapCauseReplicaIdentity:            refusal.ClassEnvironmental,
		CopySwapCauseForeignKeys:                refusal.ClassCapabilityBoundary,
		CopySwapCauseTriggers:                   refusal.ClassCapabilityBoundary,
		CopySwapCausePartitioned:                refusal.ClassCapabilityBoundary,
		CopySwapCauseUnlogged:                   refusal.ClassCapabilityBoundary,
		CopySwapCauseForceRLS:                   refusal.ClassCapabilityBoundary,
		CopySwapCauseDependentViews:             refusal.ClassCapabilityBoundary,
		CopySwapCausePublicationMember:          refusal.ClassCapabilityBoundary,
		CopySwapCauseSubscriptionTarget:         refusal.ClassCapabilityBoundary,
		CopySwapCauseDependents:                 refusal.ClassCapabilityBoundary,
		CopySwapCauseLogicalDecodingUnavailable: refusal.ClassEnvironmental,
		CopySwapCauseSlotCollision:              refusal.ClassEnvironmental,
		CopySwapCauseSlotHeadroom:               refusal.ClassEnvironmental,
		CopySwapCauseDiskHeadroom:               refusal.ClassEnvironmental,
	}
	assert.Len(t, CopySwapRefusalCauses(), len(want), "every registered cause has an expected class")
	for _, cause := range CopySwapRefusalCauses() {
		assert.Equal(t, want[cause], cause.Class(), "cause %q", cause)
		assert.Contains(t, refusal.Classes(), cause.Class(), "cause %q answers a class refusal.Classes() does not list", cause)
	}
	assert.Empty(t, CopySwapRefusalCause("").Class())
	assert.Empty(t, CopySwapRefusalCause("not-a-cause").Class())
}
