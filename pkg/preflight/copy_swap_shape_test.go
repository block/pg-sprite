package preflight

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
// be one the shape decision can actually produce: a cause that is declared,
// enumerated, and documented but never raised would pass the other
// completeness checks. Each case flips one fact of the supported shape.
func TestRefuseCopySwapShapeRaisesEveryEnumeratedCause(t *testing.T) {
	raisedBy := map[CopySwapRefusalCause]func(f *copySwapShapeFacts){
		CopySwapCauseUnlogged:        func(f *copySwapShapeFacts) { f.relpersistence = "u" },
		CopySwapCauseForceRLS:        func(f *copySwapShapeFacts) { f.forceRLS = true },
		CopySwapCausePartitioned:     func(f *copySwapShapeFacts) { f.relkind = "p" },
		CopySwapCausePKUnsupported:   func(f *copySwapShapeFacts) { f.pkType = "uuid" },
		CopySwapCauseReplicaIdentity: func(f *copySwapShapeFacts) { f.replicaIdentity = "n" },
		CopySwapCauseForeignKeys:     func(f *copySwapShapeFacts) { f.foreignKeysIn = 1 },
		CopySwapCauseTriggers:        func(f *copySwapShapeFacts) { f.rules = 1 },
	}
	for _, cause := range CopySwapRefusalCauses() {
		flip, ok := raisedBy[cause]
		require.True(t, ok, "cause %s has no fact that raises it", cause)
		facts := supportedCopySwapShape()
		flip(&facts)
		got, detail := refuseCopySwapShape(facts)
		assert.Equal(t, cause, got)
		assert.NotEmpty(t, detail)
	}
	assert.Len(t, raisedBy, len(CopySwapRefusalCauses()), "every case here must be an enumerated cause")

	cause, detail := refuseCopySwapShape(supportedCopySwapShape())
	assert.Empty(t, cause)
	assert.Empty(t, detail)
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
