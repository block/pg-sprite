package executor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// A planned drop is checked only when the statement does nothing else, or
// when it replaces the dropped constraint in place: the add under the same
// name is NOT VALID or USING INDEX, each with its own check. Any other
// company for the drop is refused before anything runs.
func TestDropsObjectShapes(t *testing.T) {
	email := &schemadiff.DroppedObject{Kind: schemadiff.DroppedConstraint, Name: "t_email_key", Definition: "UNIQUE (email)"}
	check := &schemadiff.DroppedObject{Kind: schemadiff.DroppedConstraint, Name: "t_qty_check", Definition: "CHECK ((qty > 0))"}
	pkey := &schemadiff.DroppedObject{Kind: schemadiff.DroppedConstraint, Name: "t_pkey", Definition: "PRIMARY KEY (id)"}
	cases := []struct {
		name    string
		sql     string
		dropped *schemadiff.DroppedObject
		drops   bool
		refused bool
	}{
		{name: "the drop alone", sql: `ALTER TABLE s.t DROP CONSTRAINT t_email_key`, dropped: email, drops: true},
		{name: "a key replaced by its new index",
			sql:     `ALTER TABLE s.t DROP CONSTRAINT t_email_key, ADD CONSTRAINT t_email_key UNIQUE USING INDEX t_email_key_new`,
			dropped: email, drops: true},
		{name: "a primary key swapped in under another name",
			sql:     `ALTER TABLE s.t DROP CONSTRAINT t_pkey, ADD CONSTRAINT t_id_pk PRIMARY KEY USING INDEX t_id_pk_new`,
			dropped: pkey, drops: true},
		{name: "a unique key added under another name",
			sql:     `ALTER TABLE s.t DROP CONSTRAINT t_email_key, ADD CONSTRAINT t_email_uniq UNIQUE USING INDEX t_email_uniq_new`,
			dropped: email, refused: true},
		{name: "a rule replaced NOT VALID",
			sql:     `ALTER TABLE s.t DROP CONSTRAINT t_qty_check, ADD CONSTRAINT t_qty_check CHECK (qty > 5) NOT VALID`,
			dropped: check, drops: true},
		{name: "a rule replaced and validated in one step",
			sql:     `ALTER TABLE s.t DROP CONSTRAINT t_qty_check, ADD CONSTRAINT t_qty_check CHECK (qty > 5)`,
			dropped: check, refused: true},
		{name: "a constraint of another name added",
			sql:     `ALTER TABLE s.t DROP CONSTRAINT t_qty_check, ADD CONSTRAINT t_qty_min CHECK (qty > 5) NOT VALID`,
			dropped: check, refused: true},
		{name: "the add before the drop",
			sql:     `ALTER TABLE s.t ADD CONSTRAINT t_qty_check2 CHECK (qty > 5) NOT VALID, DROP CONSTRAINT t_qty_check`,
			dropped: check, refused: true},
		{name: "a column change beside the drop",
			sql:     `ALTER TABLE s.t DROP CONSTRAINT t_qty_check, ADD COLUMN note text`,
			dropped: check, refused: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, err := statement.ParseOne(tc.sql)
			require.NoError(t, err)
			drops, err := dropsObject(st, tc.dropped)
			if tc.refused {
				require.ErrorIs(t, err, ErrInvariantViolation)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.drops, drops)
		})
	}
}

// A drop on a partitioned table reaches its partitions unless it says ONLY.
// A rule replaced in place says ONLY or not for the whole statement, so its
// add reaches the partitions exactly as far as its drop.
func TestDropSaysOnly(t *testing.T) {
	check := &schemadiff.DroppedObject{Kind: schemadiff.DroppedConstraint, Name: "t_qty_check", Definition: "CHECK ((qty > 0))"}
	for name, tc := range map[string]struct {
		sql  string
		only bool
	}{
		"the drop alone":            {sql: `ALTER TABLE s.t DROP CONSTRAINT t_qty_check`},
		"the drop alone with ONLY":  {sql: `ALTER TABLE ONLY s.t DROP CONSTRAINT t_qty_check`, only: true},
		"a rule replaced in place":  {sql: `ALTER TABLE s.t DROP CONSTRAINT t_qty_check, ADD CONSTRAINT t_qty_check CHECK (qty > 5) NOT VALID`},
		"a rule replaced with ONLY": {sql: `ALTER TABLE ONLY s.t DROP CONSTRAINT t_qty_check, ADD CONSTRAINT t_qty_check CHECK (qty > 5) NOT VALID`, only: true},
	} {
		st, err := statement.ParseOne(tc.sql)
		require.NoError(t, err, name)
		only, err := dropSaysOnly(st, check)
		require.NoError(t, err, name)
		assert.Equal(t, tc.only, only, name)
	}

	st, err := statement.ParseOne(`ALTER TABLE s.t DROP CONSTRAINT t_qty_check, ADD COLUMN note text`)
	require.NoError(t, err)
	_, err = dropSaysOnly(st, check)
	require.ErrorIs(t, err, ErrInvariantViolation, "a drop beside an unrelated operation is not checked")
}
