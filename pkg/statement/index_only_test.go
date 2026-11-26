package statement_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/statement"
)

// pg_get_indexdef prints a partitioned parent's index with ON ONLY; the
// desired-file form declares it on the whole table, with every other part
// of the definition left as the server printed it.
func TestIndexOnWholeTableRemovesOnly(t *testing.T) {
	got, err := statement.IndexOnWholeTable(
		`CREATE INDEX events_open_idx ON ONLY events USING btree (community_id, created_at DESC) WHERE (closed_at IS NULL)`)
	require.NoError(t, err)
	assert.Equal(t,
		`CREATE INDEX events_open_idx ON events USING btree (community_id, created_at DESC) WHERE closed_at IS NULL`, got)
}

// A definition without ONLY is already the whole-table form.
func TestIndexOnWholeTableKeepsPlainDefinition(t *testing.T) {
	got, err := statement.IndexOnWholeTable(`CREATE UNIQUE INDEX events_key ON events USING btree (id)`)
	require.NoError(t, err)
	assert.Equal(t, `CREATE UNIQUE INDEX events_key ON events USING btree (id)`, got)
}

// Anything but one CREATE INDEX is refused rather than reprinted.
func TestIndexOnWholeTableRefusesOtherStatements(t *testing.T) {
	_, err := statement.IndexOnWholeTable(`ALTER TABLE ONLY events ADD COLUMN note text`)
	require.ErrorIs(t, err, statement.ErrNotCreateIndex)

	_, err = statement.IndexOnWholeTable(`CREATE INDEX a ON ONLY t (x); CREATE INDEX b ON ONLY t (y)`)
	require.ErrorIs(t, err, statement.ErrNotOneStatement)
}
