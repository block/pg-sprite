package executor

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A unique index must contain, among its plain key columns, the partition key
// of every partitioned table in the tree, matched by exact name; a key that
// is an expression can never be contained. The refusal names each table, in
// tree order, and the columns it lacks, in key order.
func TestCheckUniquePartitionKeys(t *testing.T) {
	events := keyedTable{oid: 1, schema: "app", name: "events"}
	eu := keyedTable{oid: 2, schema: "app", name: "events_eu"}
	us := keyedTable{oid: 3, schema: "app", name: "events_us"}
	byRegionThenDay := []keyedTable{events, eu, us}
	keys := map[uint32][]string{1: {"region"}, 2: {"created_at"}, 3: {"created_at"}}

	t.Run("an index that contains every level's key", func(t *testing.T) {
		require.NoError(t, checkUniquePartitionKeys("app", "events_key", []string{"id", "created_at", "region"}, byRegionThenDay, keys))
	})

	t.Run("an index that contains the parent's key alone", func(t *testing.T) {
		err := checkUniquePartitionKeys("app", "events_key", []string{"region", "id"}, byRegionThenDay, keys)
		keyErr, ok := errors.AsType[*UniqueIndexPartitionKeyError](err)
		require.True(t, ok, "got %v", err)
		assert.ErrorIs(t, err, ErrUniqueIndexPartitionKey)
		assert.Equal(t, &UniqueIndexPartitionKeyError{Schema: "app", Index: "events_key", Missing: []MissingPartitionKey{
			{Schema: "app", Table: "events_eu", Columns: []string{"created_at"}},
			{Schema: "app", Table: "events_us", Columns: []string{"created_at"}},
		}}, keyErr)
	})

	t.Run("every column of a multi-column key is checked, in key order", func(t *testing.T) {
		multi := map[uint32][]string{1: {"region", "created_at", "kind"}}
		err := checkUniquePartitionKeys("app", "events_key", []string{"created_at", "id"}, []keyedTable{events}, multi)
		keyErr, ok := errors.AsType[*UniqueIndexPartitionKeyError](err)
		require.True(t, ok, "got %v", err)
		assert.Equal(t, []MissingPartitionKey{{Schema: "app", Table: "events", Columns: []string{"region", "kind"}}}, keyErr.Missing)
	})

	t.Run("a column that differs only in case is another column", func(t *testing.T) {
		cased := map[uint32][]string{1: {"Region"}}
		err := checkUniquePartitionKeys("app", "events_key", []string{"region"}, []keyedTable{events}, cased)
		keyErr, ok := errors.AsType[*UniqueIndexPartitionKeyError](err)
		require.True(t, ok, "got %v", err)
		assert.Equal(t, []MissingPartitionKey{{Schema: "app", Table: "events", Columns: []string{"Region"}}}, keyErr.Missing)
	})

	t.Run("an expression in the key", func(t *testing.T) {
		expression := map[uint32][]string{1: {"id", ""}}
		err := checkUniquePartitionKeys("app", "events_key", []string{"id"}, []keyedTable{events}, expression)
		keyErr, ok := errors.AsType[*UniqueIndexPartitionKeyError](err)
		require.True(t, ok, "got %v", err)
		assert.Equal(t, []MissingPartitionKey{{Schema: "app", Table: "events", Expression: true}}, keyErr.Missing)
	})
}

// The refusal's text names each table's missing columns and gives an
// expression key its own clause. Names are display text, unquoted, like the
// build's other errors; Schema and Table carry them as identifiers.
func TestUniqueIndexPartitionKeyErrorText(t *testing.T) {
	err := &UniqueIndexPartitionKeyError{Schema: "app", Index: "events_key", Missing: []MissingPartitionKey{
		{Schema: "app", Table: "events_eu", Columns: []string{"created_at", "kind"}},
		{Schema: "app", Table: "Odd Table", Expression: true},
	}}
	assert.Equal(t, ErrUniqueIndexPartitionKey.Error()+": unique index app.events_key cannot be created: "+
		"it lacks partition key column(s) created_at, kind of app.events_eu; "+
		"app.Odd Table is partitioned by an expression, which no unique index can contain", err.Error())
}
