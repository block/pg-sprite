package migrate

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/block/pg-sprite/pkg/suggest"
)

// A rewrite-required verdict's detail states the steps its guidance names:
// a compound statement is split, a key whose index options cannot be
// carried is built by hand and attached, and any other guidance is named
// by its code for the operator to look up.
func TestManualPathStatesTheStepsItsGuidanceNames(t *testing.T) {
	split := manualPath(suggest.GuidanceSplitStatement)
	assert.Contains(t, split, "submit each operation as its own single-operation statement")

	key := manualPath(suggest.GuidanceUniqueIndexThenConstraint)
	assert.Contains(t, key, "CREATE UNIQUE INDEX CONCURRENTLY")
	assert.Contains(t, key, "ADD CONSTRAINT ... USING INDEX")
	assert.Contains(t, key, "(guidance unique-index-then-constraint)")
	assert.NotContains(t, key, "single-operation statement", "the statement already is one")

	other := manualPath(suggest.GuidanceAddColumnThenConstraint)
	assert.Equal(t, "run the manual path its guidance names (guidance add-column-then-constraint)", other)
}
