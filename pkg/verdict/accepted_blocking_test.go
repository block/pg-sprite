package verdict

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAcceptedBlockingRegistry(t *testing.T) {
	index := ByDesign(ReasonIndexStatement).WithSite(RefusalSiteIndexSingleRelation)
	parent := CapabilityBoundary(ReasonUnsupportedPartitionedParent).WithCause(CauseParentBlockingIndexBuild)
	assert.True(t, AcceptedBlockingEligible(index))
	assert.True(t, AcceptedBlockingEligible(parent))
	assert.False(t, AcceptedBlockingEligible(ByDesign(ReasonIndexStatement).WithSite(RefusalSiteIndexOther)))
	assert.False(t, AcceptedBlockingEligible(CapabilityBoundary(ReasonRewriteRequired)))

	_, decided := AcceptedBlockingDecision(ByDesign(ReasonIndexStatement).WithSite("unknown-site"))
	assert.False(t, decided)
}
