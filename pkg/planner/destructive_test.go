package planner

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/statement"
)

// Every operation shape carries an explicit destructiveness decision, and
// exactly the four drops are destructive. A shape added to
// statement.OpKinds() without a case in classifyDestructive fails here
// instead of reaching the destructive gates with an unexamined answer.
func TestEveryOpKindIsClassifiedForDestructiveness(t *testing.T) {
	kinds := statement.OpKinds()
	require.NotEmpty(t, kinds)

	var destructive []statement.OpKind
	for _, k := range kinds {
		d, classified := classifyDestructive(k)
		require.True(t, classified, "op kind %d (%q) has no destructiveness decision",
			k, statement.Op{Kind: k}.Describe())
		if d {
			destructive = append(destructive, k)
		}
	}
	assert.ElementsMatch(t, []statement.OpKind{
		statement.OpDropColumn,
		statement.OpDropConstraint,
		statement.OpDropIndex,
		statement.OpDropNotNull,
	}, destructive)
	assert.Equal(t, destructive, DestructiveOpKinds(),
		"the exported set is the classification, in statement.OpKinds() order")
}

// A shape outside the classified set is treated as destructive, so the
// gates refuse it without the acknowledgement rather than let it run.
func TestAnUnclassifiedOpKindIsTreatedAsDestructive(t *testing.T) {
	unclassified := statement.OpKind(len(statement.OpKinds()))
	_, classified := classifyDestructive(unclassified)
	require.False(t, classified, "the probe must be outside the classified set")
	assert.True(t, destructiveOp(unclassified))
}
