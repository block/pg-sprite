// White-box tests for create-step shape checking: the refusal causes the
// create path assigns below the ParseDesired boundary, which already turns
// away the shapes that reach them, so they are provable only here.

package executor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/preflight"
)

func TestCheckCreateStepShapeAssignsCause(t *testing.T) {
	tests := []struct {
		name   string
		sql    string
		claims []string
		cause  CreateShapeCause
	}{
		{
			name:   "concurrent index build",
			sql:    "CREATE INDEX CONCURRENTLY t_v_idx ON t (v)",
			claims: []string{"t_v_idx"},
			cause:  CreateShapeConcurrently,
		},
		{
			name:  "alter table is not a create kind",
			sql:   "ALTER TABLE t ADD COLUMN v int",
			cause: CreateShapeUnsupportedKind,
		},
		{
			name:   "admitted table claims its implicit names",
			sql:    "CREATE TABLE t (id serial PRIMARY KEY)",
			claims: []string{"t", "t_id_seq", "t_pkey"},
		},
		{
			name:  "unnamed index claims nothing",
			sql:   "CREATE INDEX ON t (v)",
			cause: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			step, err := checkCreateStepShape("app", "t", tc.sql)
			require.NoError(t, err)
			assert.Equal(t, tc.claims, step.claims)
			assert.Equal(t, tc.cause, CreateShapeCauseOf(step.refusal))
			if tc.cause == "" {
				assert.NoError(t, step.refusal)
			}
		})
	}
}

// The owned-name comparison fails only on a claim the table did not
// honour: an owned name no claim predicted rides along as the server's
// replacement, never as a mismatch on its own.
func TestOwnedNameMismatch(t *testing.T) {
	tests := []struct {
		name      string
		claimed   []string
		owned     preflight.OwnedRelationNames
		missing   []string
		unclaimed []string
	}{
		{
			name:    "every claim honoured",
			claimed: []string{"t_pkey", "t_id_seq"},
			owned:   preflight.OwnedRelationNames{ConstraintIndexes: []string{"t_pkey"}, Sequences: []string{"t_id_seq"}},
		},
		{
			name:      "suffixed constraint index",
			claimed:   []string{"t_pkey", "t_id_seq"},
			owned:     preflight.OwnedRelationNames{ConstraintIndexes: []string{"t_pkey1"}, Sequences: []string{"t_id_seq"}},
			missing:   []string{"t_pkey"},
			unclaimed: []string{"t_pkey1"},
		},
		{
			name:      "suffixed sequence and index, sorted",
			claimed:   []string{"t_pkey", "t_id_seq"},
			owned:     preflight.OwnedRelationNames{ConstraintIndexes: []string{"t_pkey1"}, Sequences: []string{"t_id_seq1"}},
			missing:   []string{"t_id_seq", "t_pkey"},
			unclaimed: []string{"t_id_seq1", "t_pkey1"},
		},
		{
			name:    "owned name nobody claimed is not a mismatch",
			claimed: []string{"t_pkey"},
			owned:   preflight.OwnedRelationNames{ConstraintIndexes: []string{"t_pkey", "t_v_key"}},
			// Nothing missing, so the caller passes; the unclaimed name is
			// reported for the record only.
			unclaimed: []string{"t_v_key"},
		},
		{
			name:  "no claims and no owned names",
			owned: preflight.OwnedRelationNames{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			missing, unclaimed := ownedNameMismatch(tc.claimed, tc.owned)
			assert.Equal(t, tc.missing, missing)
			assert.Equal(t, tc.unclaimed, unclaimed)
		})
	}
}
