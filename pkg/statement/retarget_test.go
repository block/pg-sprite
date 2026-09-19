package statement_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/statement"
)

func TestRetargetRelation(t *testing.T) {
	for _, sql := range []string{
		`ALTER TABLE public.t ALTER COLUMN id TYPE bigint`,
		`ALTER TABLE t ALTER COLUMN id TYPE bigint`,
		`ALTER TABLE ONLY t ALTER COLUMN id TYPE bigint`,
		`ALTER TABLE IF EXISTS t ALTER COLUMN id TYPE bigint`,
		`ALTER TABLE t ADD CONSTRAINT c CHECK (note <> 'other.t')`,
		`ALTER TABLE t ADD CONSTRAINT fk FOREIGN KEY (id) REFERENCES other.t(id)`,
	} {
		t.Run(sql, func(t *testing.T) {
			got, err := statement.RetargetRelation(sql, "s", "shadow")
			require.NoError(t, err)
			assert.Contains(t, got, "s.shadow")
			require.NoError(t, statement.SameOpsExceptTarget(sql, got))
		})
	}
}

func TestRetargetRelationRefusesInvalidShape(t *testing.T) {
	_, err := statement.RetargetRelation(`SELECT 1`, "s", "t")
	require.ErrorIs(t, err, statement.ErrNotAlterTable)
	_, err = statement.RetargetRelation(`ALTER TABLE t ADD COLUMN a int; ALTER TABLE t ADD COLUMN b int`, "s", "t")
	require.ErrorIs(t, err, statement.ErrNotOneStatement)
}

func TestSameOpsExceptTargetRejectsTampering(t *testing.T) {
	err := statement.SameOpsExceptTarget(`ALTER TABLE t ALTER COLUMN id TYPE bigint`, `ALTER TABLE shadow ALTER COLUMN other TYPE bigint`)
	require.ErrorIs(t, err, statement.ErrRetargetMismatch)
}

func TestSameOpsExceptTargetRejectsSemanticChanges(t *testing.T) {
	tests := map[string]struct{ gated, changed string }{
		"check expression":    {`ALTER TABLE t ADD CONSTRAINT c CHECK (qty > 0)`, `ALTER TABLE shadow ADD CONSTRAINT c CHECK (qty > 999)`},
		"default literal":     {`ALTER TABLE t ALTER COLUMN qty SET DEFAULT 1`, `ALTER TABLE shadow ALTER COLUMN qty SET DEFAULT 2`},
		"referenced relation": {`ALTER TABLE t ADD CONSTRAINT fk FOREIGN KEY (parent_id) REFERENCES parents(id)`, `ALTER TABLE shadow ADD CONSTRAINT fk FOREIGN KEY (parent_id) REFERENCES other_parents(id)`},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			require.ErrorIs(t, statement.SameOpsExceptTarget(tc.gated, tc.changed), statement.ErrRetargetMismatch)
		})
	}
}

func TestSameOpsExceptTargetAcceptsGenuineRetarget(t *testing.T) {
	gated := `ALTER TABLE public.t ADD CONSTRAINT c CHECK (qty > 0)`
	retargeted, err := statement.RetargetRelation(gated, "work", "shadow")
	require.NoError(t, err)
	require.NoError(t, statement.SameOpsExceptTarget(gated, retargeted))
}
