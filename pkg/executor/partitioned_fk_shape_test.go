package executor_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
)

// AdmitPartitionedForeignKeyShape needs no connection, so a dry run learns
// every shape refusal the partitioned foreign key build would return before
// anything runs. It admits one named FOREIGN KEY on a schema-qualified table,
// referencing a schema-qualified table's listed columns, with any MATCH
// SIMPLE or FULL, referential actions, ON DELETE column list, and
// deferrability: the build renders
// each partition's statement and the parent's from those fields, so every
// partition's foreign key has the parent's definition.
func TestAdmitPartitionedForeignKeyShape(t *testing.T) {
	for _, sql := range []string{
		`ALTER TABLE app.events ADD CONSTRAINT events_account_fk FOREIGN KEY (account_id) REFERENCES app.accounts (id)`,
		`ALTER TABLE app.events
		     ADD CONSTRAINT events_account_fk FOREIGN KEY (account_id, region) REFERENCES app.accounts (id, region)
		     MATCH FULL ON UPDATE CASCADE ON DELETE SET NULL DEFERRABLE INITIALLY DEFERRED`,
		`ALTER TABLE app.events
		     ADD CONSTRAINT events_account_fk FOREIGN KEY (account_id, region) REFERENCES app.accounts (id, region)
		     ON DELETE SET DEFAULT (account_id)`,
	} {
		assert.NoError(t, executor.AdmitPartitionedForeignKeyShape(sql), sql)
	}
}

// Each refusal is typed, so a caller branches on the cause.
func TestAdmitPartitionedForeignKeyShapeRefusals(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want error
	}{
		{
			name: "not an ALTER TABLE",
			sql:  `CREATE INDEX events_account_idx ON app.events (account_id)`,
			want: executor.ErrNotAddForeignKey,
		},
		{
			name: "a key constraint, not a foreign key",
			sql:  `ALTER TABLE app.events ADD CONSTRAINT events_pkey PRIMARY KEY (id, created_at)`,
			want: executor.ErrNotAddForeignKey,
		},
		{
			name: "a second subcommand",
			sql: `ALTER TABLE app.events
			          ADD CONSTRAINT events_account_fk FOREIGN KEY (account_id) REFERENCES app.accounts (id),
			          ADD COLUMN note text`,
			want: executor.ErrNotAddForeignKey,
		},
		{
			name: "an unqualified table",
			sql:  `ALTER TABLE events ADD CONSTRAINT events_account_fk FOREIGN KEY (account_id) REFERENCES app.accounts (id)`,
			want: executor.ErrUnqualifiedTable,
		},
		{
			// Each partition's foreign key takes a name derived from this
			// one, and a rerun finds it by that name.
			name: "an unnamed foreign key",
			sql:  `ALTER TABLE app.events ADD FOREIGN KEY (account_id) REFERENCES app.accounts (id)`,
			want: executor.ErrPartitionedForeignKeyUnsupportedShape,
		},
		{
			// PostgreSQL 14 to 17 refuse it on a partitioned table; the build
			// exists to add the foreign key validated without it.
			name: "NOT VALID",
			sql:  `ALTER TABLE app.events ADD CONSTRAINT events_account_fk FOREIGN KEY (account_id) REFERENCES app.accounts (id) NOT VALID`,
			want: executor.ErrPartitionedForeignKeyUnsupportedShape,
		},
		{
			name: "ONLY",
			sql:  `ALTER TABLE ONLY app.events ADD CONSTRAINT events_account_fk FOREIGN KEY (account_id) REFERENCES app.accounts (id)`,
			want: executor.ErrPartitionedForeignKeyUnsupportedShape,
		},
		{
			// Every partition's statement would resolve the name through its
			// own session's search_path.
			name: "an unqualified referenced table",
			sql:  `ALTER TABLE app.events ADD CONSTRAINT events_account_fk FOREIGN KEY (account_id) REFERENCES accounts (id)`,
			want: executor.ErrPartitionedForeignKeyUnsupportedShape,
		},
		{
			// The referenced columns are what the build compares on every
			// partition, so they are named rather than left to the primary key.
			name: "no referenced column list",
			sql:  `ALTER TABLE app.events ADD CONSTRAINT events_account_fk FOREIGN KEY (account_id) REFERENCES app.accounts`,
			want: executor.ErrPartitionedForeignKeyUnsupportedShape,
		},
		{
			// The server refuses it too: only the foreign key's own columns
			// can be set on delete.
			name: "an ON DELETE column outside the foreign key",
			sql: `ALTER TABLE app.events
			          ADD CONSTRAINT events_account_fk FOREIGN KEY (account_id, region) REFERENCES app.accounts (id, region)
			          ON DELETE SET NULL (note)`,
			want: executor.ErrPartitionedForeignKeyUnsupportedShape,
		},
		{
			name: "an ON DELETE column named twice",
			sql: `ALTER TABLE app.events
			          ADD CONSTRAINT events_account_fk FOREIGN KEY (account_id, region) REFERENCES app.accounts (id, region)
			          ON DELETE SET NULL (account_id, account_id)`,
			want: executor.ErrPartitionedForeignKeyUnsupportedShape,
		},
		{
			name: "a repeated column",
			sql: `ALTER TABLE app.events
			          ADD CONSTRAINT events_account_fk FOREIGN KEY (account_id, account_id) REFERENCES app.accounts (id, region)`,
			want: executor.ErrPartitionedForeignKeyUnsupportedShape,
		},
		{
			name: "fewer referenced columns than referencing ones",
			sql: `ALTER TABLE app.events
			          ADD CONSTRAINT events_account_fk FOREIGN KEY (account_id, region) REFERENCES app.accounts (id)`,
			want: executor.ErrPartitionedForeignKeyUnsupportedShape,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := executor.AdmitPartitionedForeignKeyShape(tt.sql)
			require.ErrorIs(t, err, tt.want)
		})
	}
}
