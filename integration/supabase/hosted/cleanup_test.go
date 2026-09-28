package hosted_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Register before the first write: the CLI can commit DDL and then fail to
// return valid output. Never take cleanup ownership of an existing relation.
func (f *fixture) prepareTableCleanup(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	var exists bool
	require.NoError(t, f.pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", f.table).Scan(&exists))
	require.False(t, exists, "fixture relation must not already exist")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 15*time.Second)
		defer cancel()
		_, err := f.pool.Exec(ctx, "DROP TABLE IF EXISTS "+f.table)
		assert.NoError(t, err)
	})
}

// Cleanup covers a command that never created its table and one that committed
// the table before a later error. The same hosted opt-in and project checks apply.
func TestHostedTableCleanup(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	t.Run("no committed table", func(t *testing.T) {
		f.prepareTableCleanup(t)
	})
	t.Run("table committed before failure", func(t *testing.T) {
		f.prepareTableCleanup(t)
		_, err := f.pool.Exec(ctx, "CREATE TABLE "+f.table+" (id integer PRIMARY KEY)")
		require.NoError(t, err)
		_, err = f.pool.Exec(ctx, "SELECT 1 / 0")
		require.Error(t, err)
	})
	var exists bool
	require.NoError(t, f.pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", f.table).Scan(&exists))
	assert.False(t, exists, "cleanup removes the committed table despite the later failure")
}
