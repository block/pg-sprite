package executor_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/statement"
)

// AdmitPartitionedIndexShape needs no connection, so a dry run learns every
// shape refusal the build would return before anything runs. It admits one
// named CREATE INDEX on a schema-qualified table, with or without ONLY and
// CONCURRENTLY, and refuses every other shape with the build's own sentinel.
func TestAdmitPartitionedIndexShape(t *testing.T) {
	for _, sql := range []string{
		"CREATE INDEX events_at_idx ON app.events (at)",
		"CREATE INDEX events_at_idx ON ONLY app.events (at)",
		"CREATE INDEX CONCURRENTLY events_at_idx ON app.events (at)",
		"CREATE UNIQUE INDEX events_id_idx ON app.events (id, at)",
	} {
		t.Run("admits "+sql, func(t *testing.T) {
			require.NoError(t, executor.AdmitPartitionedIndexShape(sql))
		})
	}

	for _, tc := range []struct {
		name string
		sql  string
		want error
	}{
		{"not a CREATE INDEX", "ALTER TABLE app.events ADD COLUMN note text", executor.ErrNotCreateIndex},
		{"an unnamed index", "CREATE INDEX ON app.events (at)", executor.ErrUnnamedIndex},
		{"IF NOT EXISTS", "CREATE INDEX IF NOT EXISTS events_at_idx ON app.events (at)", executor.ErrIfNotExistsUnsupported},
		{"a TABLESPACE clause", "CREATE INDEX events_at_idx ON app.events (at) TABLESPACE fast", executor.ErrPartitionedIndexTablespace},
		{"an unqualified table", "CREATE INDEX events_at_idx ON events (at)", executor.ErrUnqualifiedTable},
		{"two statements", "CREATE INDEX a_idx ON app.events (at); CREATE INDEX b_idx ON app.events (id)", statement.ErrNotOneStatement},
	} {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			assert.ErrorIs(t, executor.AdmitPartitionedIndexShape(tc.sql), tc.want)
		})
	}

	t.Run("refuses a statement that does not parse", func(t *testing.T) {
		require.Error(t, executor.AdmitPartitionedIndexShape("CREATE INDEX events_at_idx ON app.events ("))
	})
}
