package executor_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/statement"
)

// AdmitPartitionedKeyShape needs no connection, so a dry run learns every
// shape refusal the partitioned key build would return before anything runs.
// It admits one named PRIMARY KEY or UNIQUE constraint over a plain list of
// distinct columns on a schema-qualified table. The build renders the
// parent's ADD CONSTRAINT from the kind, the name, and the columns alone, so
// every other clause is refused rather than dropped.
func TestAdmitPartitionedKeyShape(t *testing.T) {
	for _, sql := range []string{
		"ALTER TABLE app.events ADD CONSTRAINT events_pkey PRIMARY KEY (id, created_at)",
		"ALTER TABLE app.events ADD CONSTRAINT events_id_key UNIQUE (id, created_at)",
		// NOT DEFERRABLE and INITIALLY IMMEDIATE spell the defaults.
		"ALTER TABLE app.events ADD CONSTRAINT events_pkey PRIMARY KEY (id, created_at) NOT DEFERRABLE INITIALLY IMMEDIATE",
	} {
		t.Run("admits "+sql, func(t *testing.T) {
			require.NoError(t, executor.AdmitPartitionedKeyShape(sql))
		})
	}

	for _, tc := range []struct {
		name string
		sql  string
		want error
	}{
		{"a CREATE INDEX", "CREATE UNIQUE INDEX events_id_idx ON app.events (id, created_at)", executor.ErrNotAddKeyConstraint},
		{"a CHECK constraint", "ALTER TABLE app.events ADD CONSTRAINT events_id_check CHECK (id > 0)", executor.ErrNotAddKeyConstraint},
		{"a key added inline with a column", "ALTER TABLE app.events ADD COLUMN code text UNIQUE", executor.ErrNotAddKeyConstraint},
		{
			"two subcommands",
			"ALTER TABLE app.events ADD CONSTRAINT events_pkey PRIMARY KEY (id, created_at), ADD CONSTRAINT events_id_key UNIQUE (id, created_at)",
			executor.ErrNotAddKeyConstraint,
		},
		{"an unqualified table", "ALTER TABLE events ADD CONSTRAINT events_pkey PRIMARY KEY (id, created_at)", executor.ErrUnqualifiedTable},
		// Each partition's index and constraint take names derived from the
		// constraint's name, and a rerun finds them by those names.
		{"an unnamed constraint", "ALTER TABLE app.events ADD PRIMARY KEY (id, created_at)", executor.ErrPartitionedKeyUnsupportedShape},
		{"INCLUDE", "ALTER TABLE app.events ADD CONSTRAINT events_pkey PRIMARY KEY (id, created_at) INCLUDE (payload)", executor.ErrPartitionedKeyUnsupportedShape},
		{"NULLS NOT DISTINCT", "ALTER TABLE app.events ADD CONSTRAINT events_id_key UNIQUE NULLS NOT DISTINCT (id, created_at)", executor.ErrPartitionedKeyUnsupportedShape},
		{"DEFERRABLE", "ALTER TABLE app.events ADD CONSTRAINT events_id_key UNIQUE (id, created_at) DEFERRABLE", executor.ErrPartitionedKeyUnsupportedShape},
		{
			"INITIALLY DEFERRED",
			"ALTER TABLE app.events ADD CONSTRAINT events_id_key UNIQUE (id, created_at) DEFERRABLE INITIALLY DEFERRED",
			executor.ErrPartitionedKeyUnsupportedShape,
		},
		{"WITH storage parameters", "ALTER TABLE app.events ADD CONSTRAINT events_pkey PRIMARY KEY (id, created_at) WITH (fillfactor = 70)", executor.ErrPartitionedKeyUnsupportedShape},
		{"USING INDEX TABLESPACE", "ALTER TABLE app.events ADD CONSTRAINT events_pkey PRIMARY KEY (id, created_at) USING INDEX TABLESPACE fast", executor.ErrPartitionedKeyUnsupportedShape},
		{"USING INDEX", "ALTER TABLE app.events ADD CONSTRAINT events_pkey PRIMARY KEY USING INDEX events_pkey_idx", executor.ErrPartitionedKeyUnsupportedShape},
		// ONLY would add the parent's constraint and leave the partitions'.
		{"ONLY", "ALTER TABLE ONLY app.events ADD CONSTRAINT events_pkey PRIMARY KEY (id, created_at)", executor.ErrPartitionedKeyUnsupportedShape},
		{"a repeated column", "ALTER TABLE app.events ADD CONSTRAINT events_id_key UNIQUE (id, created_at, id)", executor.ErrPartitionedKeyUnsupportedShape},
		{
			"two statements",
			"ALTER TABLE app.events ADD CONSTRAINT events_pkey PRIMARY KEY (id, created_at); ALTER TABLE app.events ADD CONSTRAINT events_id_key UNIQUE (id, created_at)",
			statement.ErrNotOneStatement,
		},
	} {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			assert.ErrorIs(t, executor.AdmitPartitionedKeyShape(tc.sql), tc.want)
		})
	}

	// The grammar itself refuses NOT VALID on a key constraint, at the parse
	// boundary, before the shape is read.
	t.Run("refuses NOT VALID", func(t *testing.T) {
		require.Error(t, executor.AdmitPartitionedKeyShape("ALTER TABLE app.events ADD CONSTRAINT events_id_key UNIQUE (id, created_at) NOT VALID"))
	})

	t.Run("refuses a statement that does not parse", func(t *testing.T) {
		require.Error(t, executor.AdmitPartitionedKeyShape("ALTER TABLE app.events ADD CONSTRAINT events_pkey PRIMARY KEY ("))
	})
}
