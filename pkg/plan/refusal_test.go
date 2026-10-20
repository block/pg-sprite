package plan_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/verdict"
)

// The registry classifies each typed cause explicitly rather than reading the
// cause's own Class(); these tests pin the two answers to each other so the
// registry cannot drift from the class the cause declares.

func TestCreateShapeRefusalClassMatchesCauseClass(t *testing.T) {
	for _, cause := range executor.CreateShapeCauses() {
		r, ok := plan.CreateShapeRefusal(cause)
		require.True(t, ok, cause)
		assert.Equal(t, verdict.Class(cause.Class()), r.Class(), cause)
	}
}

func TestPartitionRefusalClassMatchesCauseClass(t *testing.T) {
	for _, cause := range preflight.PartitionRefusalCauses() {
		r, ok := plan.PartitionRefusal(cause)
		require.True(t, ok, cause)
		assert.Equal(t, verdict.Class(cause.Class()), r.Class(), cause)
	}
}
