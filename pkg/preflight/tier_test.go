package preflight_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/preflight"
)

func TestRequiredTier(t *testing.T) {
	tests := []struct {
		name    string
		execSQL []string
		tier    preflight.Tier
	}{
		{
			name:    "in-place alter is owner-gated",
			execSQL: []string{"ALTER TABLE s.t ADD COLUMN c int"},
			tier:    preflight.TierAlterInPlace,
		},
		{
			name:    "concurrent index build needs schema CREATE",
			execSQL: []string{"CREATE INDEX CONCURRENTLY i ON s.t (c)"},
			tier:    preflight.TierIndexBuild,
		},
		{
			name: "mixed sequence takes the most demanding step's tier",
			execSQL: []string{
				"ALTER TABLE s.t ADD COLUMN c int",
				"CREATE UNIQUE INDEX CONCURRENTLY t_c_key ON s.t (c)",
				"ALTER TABLE s.t ADD CONSTRAINT t_c_key UNIQUE USING INDEX t_c_key",
			},
			tier: preflight.TierIndexBuild,
		},
		// The ALTER TABLE shapes that build a new index as a side effect
		// need schema CREATE exactly like an explicit CREATE INDEX — the
		// server refuses them with "permission denied for schema" on
		// ownership alone.
		{
			name:    "add unique constraint builds its backing index",
			execSQL: []string{"ALTER TABLE s.t ADD CONSTRAINT t_c_key UNIQUE (c)"},
			tier:    preflight.TierIndexBuild,
		},
		{
			name:    "add primary key builds its backing index",
			execSQL: []string{"ALTER TABLE s.t ADD CONSTRAINT t_pkey PRIMARY KEY (c)"},
			tier:    preflight.TierIndexBuild,
		},
		{
			name:    "add exclusion constraint builds its backing index",
			execSQL: []string{"ALTER TABLE s.t ADD CONSTRAINT t_excl EXCLUDE USING gist (c WITH =)"},
			tier:    preflight.TierIndexBuild,
		},
		{
			name:    "add column with inline unique builds its backing index",
			execSQL: []string{"ALTER TABLE s.t ADD COLUMN e int UNIQUE"},
			tier:    preflight.TierIndexBuild,
		},
		// The counterparts that must stay owner-gated: USING INDEX adopts
		// an existing index, and a rewriting type change only rebuilds
		// existing indexes — the server allows both without schema CREATE.
		{
			name:    "using index adoption builds nothing",
			execSQL: []string{"ALTER TABLE s.t ADD CONSTRAINT t_c_key UNIQUE USING INDEX t_c_key"},
			tier:    preflight.TierAlterInPlace,
		},
		{
			name:    "rewriting type change rebuilds existing indexes only",
			execSQL: []string{"ALTER TABLE s.t ALTER COLUMN c TYPE bigint"},
			tier:    preflight.TierAlterInPlace,
		},
		// The greenfield create plan derives the off-ladder create tier —
		// checked by CheckCreatePrivileges, never by the ladder walk —
		// whether or not the plan also builds the new table's indexes.
		{
			name:    "create table derives the off-ladder create tier",
			execSQL: []string{"CREATE TABLE s.t (id int)"},
			tier:    preflight.TierCreateTable,
		},
		{
			name: "create plan with index builds stays the create tier",
			execSQL: []string{
				"CREATE TABLE s.t (id int, c int)",
				"CREATE INDEX t_c_idx ON s.t (c)",
			},
			tier: preflight.TierCreateTable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tier, err := preflight.RequiredTier(tt.execSQL)
			require.NoError(t, err)
			assert.Equal(t, tt.tier, tier)
		})
	}

	t.Run("a step shape the engine does not execute fails closed", func(t *testing.T) {
		_, err := preflight.RequiredTier([]string{"DROP INDEX CONCURRENTLY i"})
		require.Error(t, err)
	})

	t.Run("an unparseable step fails closed", func(t *testing.T) {
		_, err := preflight.RequiredTier([]string{"not sql at all"})
		require.Error(t, err)
	})

	t.Run("a set mixing create table with alter table fails closed", func(t *testing.T) {
		// The off-ladder create tier proves creation access only; it
		// cannot vouch for the ladder rungs an existing-table alter needs,
		// and no front door produces such a set.
		_, err := preflight.RequiredTier([]string{
			"CREATE TABLE s.t (id int)",
			"ALTER TABLE s.u ADD COLUMN c int",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "mixes CREATE TABLE with ALTER TABLE")
	})
}
