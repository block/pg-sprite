package statement_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/statement"
)

// IndexKeyColumns lists the columns an index's key names, the way PostgreSQL
// counts them when it checks a unique index against a partition key: a
// column written in parentheses, or under COLLATE, is still that column; an
// expression over a column and an INCLUDE column are not key columns.
func TestIndexKeyColumns(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want []string
	}{
		{
			"plain columns in key order",
			`CREATE UNIQUE INDEX events_key ON app.events (community_id, kind)`,
			[]string{"community_id", "kind"},
		},
		{
			"a quoted column keeps its case",
			`CREATE UNIQUE INDEX events_key ON app.events ("CreatedAt", id)`,
			[]string{"CreatedAt", "id"},
		},
		{
			"a parenthesized column, with and without COLLATE, and a qualified one",
			`CREATE UNIQUE INDEX events_key ON app.events ((id), (region COLLATE "C"), (events.created_at))`,
			[]string{"id", "region", "created_at"},
		},
		{
			"an expression and the INCLUDE columns are not key columns",
			`CREATE UNIQUE INDEX events_key ON app.events (id, lower(kind), (created_at + interval '1 day'))
			 INCLUDE (region)`,
			[]string{"id"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := statement.IndexKeyColumns(tt.sql)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	t.Run("refuses a statement that is not a CREATE INDEX", func(t *testing.T) {
		_, err := statement.IndexKeyColumns(`ALTER TABLE app.events ADD COLUMN note text`)
		assert.ErrorIs(t, err, statement.ErrNotCreateIndex)
	})
}
