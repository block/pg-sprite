package preflight_test

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/block/pg-sprite/pkg/preflight"
)

// The target answers "may this run create a slot" through one accessor
// only: DecodesWAL, the mode the cluster was verified for. The role proof's
// replication bit stays on the shape, where the environment check reads
// it, so a quiesced target minted from a replication-verified shape cannot
// report two run modes that disagree.
func TestCopySwapTargetReportsOnlyTheVerifiedRunMode(t *testing.T) {
	target := reflect.TypeFor[preflight.CopySwapTarget]()
	_, hasDecodesWAL := target.MethodByName("DecodesWAL")
	_, hasLogicalDecoding := target.MethodByName("LogicalDecoding")
	assert.True(t, hasDecodesWAL)
	assert.False(t, hasLogicalDecoding, "a quiesced target minted from a replication-verified shape would report LogicalDecoding() true")

	shape := reflect.TypeFor[preflight.CopySwapShape]()
	_, shapeHasLogicalDecoding := shape.MethodByName("LogicalDecoding")
	_, shapeHasDecodesWAL := shape.MethodByName("DecodesWAL")
	assert.True(t, shapeHasLogicalDecoding, "the environment check reads the role proof's replication bit from the shape")
	assert.False(t, shapeHasDecodesWAL, "no run mode is verified before the environment check")
}
