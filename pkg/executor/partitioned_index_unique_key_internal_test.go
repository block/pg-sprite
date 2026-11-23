package executor

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A unique index must include every partition key column of every
// partitioned table in the tree, matched by name; a key that is an
// expression cannot be included at all, whatever columns the index has.
func TestCheckUniquePartitionKeys(t *testing.T) {
	byRegionThenDay := []partitionKey{
		{table: "app.events", columns: []string{"region"}},
		{table: "app.events_eu", columns: []string{"created_at"}},
		{table: "app.events_us", columns: []string{"created_at"}},
	}

	t.Run("an index that covers every level", func(t *testing.T) {
		require.NoError(t, checkUniquePartitionKeys("app.events_key", []string{"id", "created_at", "region"}, byRegionThenDay))
	})

	t.Run("an index that covers the parent alone", func(t *testing.T) {
		err := checkUniquePartitionKeys("app.events_key", []string{"region", "id"}, byRegionThenDay)
		keyErr, ok := errors.AsType[*UniqueIndexPartitionKeyError](err)
		require.True(t, ok, "got %v", err)
		assert.ErrorIs(t, err, ErrUniqueIndexMissingPartitionKey)
		assert.Equal(t, "app.events_key", keyErr.Index)
		assert.Equal(t, []MissingPartitionKey{
			{Table: "app.events_eu", Columns: []string{"created_at"}},
			{Table: "app.events_us", Columns: []string{"created_at"}},
		}, keyErr.Missing)
	})

	t.Run("an expression in the key", func(t *testing.T) {
		keys := []partitionKey{{table: "app.events", columns: []string{"id"}, expression: true}}
		err := checkUniquePartitionKeys("app.events_key", []string{"id"}, keys)
		keyErr, ok := errors.AsType[*UniqueIndexPartitionKeyError](err)
		require.True(t, ok, "got %v", err)
		assert.Equal(t, []MissingPartitionKey{{Table: "app.events", Expression: true}}, keyErr.Missing)
	})
}
