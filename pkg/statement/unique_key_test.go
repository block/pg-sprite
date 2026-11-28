package statement

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Two unique indexes share a key when they reject duplicates over the same
// columns, whatever their names, predicates, and INCLUDE columns.
func TestUniqueIndexKey(t *testing.T) {
	key := func(sql string) (string, bool) {
		t.Helper()
		k, ok, err := UniqueIndexKey(sql)
		require.NoError(t, err)
		return k, ok
	}
	base, ok := key("CREATE UNIQUE INDEX foo ON events USING btree (email)")
	require.True(t, ok)
	for _, sql := range []string{
		"CREATE UNIQUE INDEX bar ON events USING btree (email)",
		"CREATE UNIQUE INDEX bar ON events USING btree (email) WHERE (deleted_at IS NULL)",
		"CREATE UNIQUE INDEX CONCURRENTLY bar ON events USING btree (email) INCLUDE (name)",
	} {
		got, ok := key(sql)
		assert.True(t, ok, sql)
		assert.Equal(t, base, got, sql)
	}
	other, ok := key("CREATE UNIQUE INDEX bar ON events USING btree (email, name)")
	assert.True(t, ok)
	assert.NotEqual(t, base, other, "another key")

	_, ok = key("CREATE INDEX bar ON events USING btree (email)")
	assert.False(t, ok, "a non-unique index enforces no rule")

	_, _, err := UniqueIndexKey("ALTER TABLE events ADD COLUMN c int")
	assert.ErrorIs(t, err, ErrNotCreateIndex)
}
