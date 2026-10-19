package planner_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/planner"
)

// A script classified in order numbers an unnamed constraint past the
// names its earlier statements claim, whether stated or chosen by a safer
// sequence, frees a name an earlier statement drops, and keeps schemas
// apart.
func TestScriptClassifierNumbersPastNamesTheScriptClaims(t *testing.T) {
	cases := []struct {
		name   string
		script []string
		want   string
	}{
		{
			name:   "first statement takes the first choice",
			script: []string{"ALTER TABLE t ADD CHECK (qty > 0)"},
			want:   "t_qty_check",
		},
		{
			name:   "a name an earlier rewrite chose",
			script: []string{"ALTER TABLE t ADD CHECK (qty > 0)", "ALTER TABLE t ADD CHECK (qty < 100)"},
			want:   "t_qty_check1",
		},
		{
			name:   "a name an earlier statement states",
			script: []string{"ALTER TABLE t ADD CONSTRAINT t_qty_check CHECK (qty > 0) NOT VALID", "ALTER TABLE t ADD CHECK (qty < 100)"},
			want:   "t_qty_check1",
		},
		{
			name: "a name an earlier statement dropped",
			script: []string{
				"ALTER TABLE t ADD CHECK (qty > 0)",
				"ALTER TABLE t DROP CONSTRAINT t_qty_check",
				"ALTER TABLE t ADD CHECK (qty < 100)",
			},
			want: "t_qty_check",
		},
		{
			name:   "a name claimed in another schema",
			script: []string{"ALTER TABLE app.t ADD CHECK (qty > 0)", "ALTER TABLE t ADD CHECK (qty < 100)"},
			want:   "t_qty_check",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var classifier planner.ScriptClassifier
			var last planner.Plan
			for _, sql := range tc.script {
				plan, err := classifier.Classify(sql)
				require.NoError(t, err, sql)
				last = plan
			}
			require.Len(t, last.Decisions, 1)
			require.Len(t, last.Decisions[0].SaferSQL, 2)
			assert.Contains(t, last.Decisions[0].SaferSQL[0], "ADD CONSTRAINT "+tc.want+" CHECK")
			assert.Contains(t, last.Decisions[0].SaferSQL[1], `VALIDATE CONSTRAINT "`+tc.want+`"`)
		})
	}
}
