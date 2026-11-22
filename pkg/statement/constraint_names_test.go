package statement_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/statement"
)

func TestConstraintNames(t *testing.T) {
	cases := []struct {
		name     string
		sql      string
		names    []string
		complete bool
	}{
		{
			name: "create table lists stated names only",
			sql: `CREATE TABLE t (
				id bigint PRIMARY KEY,
				qty int CONSTRAINT qty_positive CHECK (qty > 0),
				lo int, hi int,
				CHECK (lo < hi),
				CONSTRAINT lo_hi_set CHECK ((lo IS NULL) = (hi IS NULL)),
				CONSTRAINT t_hi_key UNIQUE (hi)
			)`,
			names:    []string{"qty_positive", "lo_hi_set", "t_hi_key"},
			complete: true,
		},
		{
			name:     "alter table add constraint",
			sql:      `ALTER TABLE t ADD CONSTRAINT lo_positive CHECK (lo > 0), ADD CHECK (hi > 0)`,
			names:    []string{"lo_positive"},
			complete: true,
		},
		{
			name:     "alter table add column with an inline constraint",
			sql:      `ALTER TABLE t ADD COLUMN mid int CONSTRAINT mid_positive CHECK (mid > 0)`,
			names:    []string{"mid_positive"},
			complete: true,
		},
		{
			name:     "like copying constraints is incomplete",
			sql:      `CREATE TABLE t (LIKE other INCLUDING ALL, CHECK (id > 0))`,
			complete: false,
		},
		{
			name:     "like excluding constraints stays complete",
			sql:      `CREATE TABLE t (LIKE other INCLUDING ALL EXCLUDING CONSTRAINTS, CHECK (id > 0))`,
			complete: true,
		},
		{
			name:     "other statements declare nothing",
			sql:      `CREATE INDEX t_lo_idx ON t (lo)`,
			complete: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := statement.ConstraintNames(tc.sql)
			require.NoError(t, err)
			assert.Equal(t, tc.names, got.Names)
			assert.Equal(t, tc.complete, got.Complete)
		})
	}

	_, err := statement.ConstraintNames(`SELECT 1; SELECT 2`)
	require.ErrorIs(t, err, statement.ErrNotOneStatement)
}
