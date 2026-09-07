// White-box tests for create-step shape checking: the refusal causes the
// create path assigns below the ParseDesired boundary, which already turns
// away the shapes that reach them, so they are provable only here.

package executor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
