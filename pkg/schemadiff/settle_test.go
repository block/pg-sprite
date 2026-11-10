package schemadiff

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A replay changes only the text of a definition. Anything else that differs
// between two introspections of the scratch table is refused rather than
// recorded as a replay.
func TestRecordReplaysRefusesStructuralChanges(t *testing.T) {
	before := Model{
		Table:       "t",
		Columns:     []Column{{Name: "a"}, {Name: "b"}},
		Constraints: []Constraint{{Name: "ck", Def: "CHECK (a > 0)"}},
		Indexes:     []Index{{Name: "t_idx", Def: "CREATE INDEX t_idx ON t USING btree (a) WHERE (a > 0)"}},
	}
	for name, tc := range map[string]struct {
		after func(Model) Model
		want  string
	}{
		"a column dropped": {
			after: func(m Model) Model { m.Columns = m.Columns[:1]; return m },
			want:  "replaying the desired expressions of t changed its columns, constraints, or indexes",
		},
		"columns reordered": {
			after: func(m Model) Model { m.Columns = []Column{{Name: "b"}, {Name: "a"}}; return m },
			want:  "replaying the desired expressions of t reordered its columns",
		},
		"a constraint renamed": {
			after: func(m Model) Model { m.Constraints = []Constraint{{Name: "ck2", Def: "CHECK (a > 0)"}}; return m },
			want:  "replaying the desired expressions of t renamed a constraint",
		},
		"an index renamed": {
			after: func(m Model) Model { m.Indexes = []Index{{Name: "t_idx2", Def: before.Indexes[0].Def}}; return m },
			want:  "replaying the desired expressions of t renamed an index",
		},
	} {
		t.Run(name, func(t *testing.T) {
			settled := cloneReplayed(before)
			assert.EqualError(t, recordReplays(&settled, before, tc.after(cloneReplayed(before))), tc.want)
		})
	}
}

// Each changed text is appended to the settled model's replays, and the
// model it started from is left alone.
func TestRecordReplaysAppendsChangedTexts(t *testing.T) {
	before := Model{
		Table:       "t",
		Columns:     []Column{{Name: "a", Default: "1"}, {Name: "b", Default: "2"}},
		Constraints: []Constraint{{Name: "ck", Def: "CHECK (x)"}},
		Indexes:     []Index{{Name: "t_idx", Def: "CREATE INDEX t_idx ON t USING btree (a) WHERE x"}},
	}
	after := cloneReplayed(before)
	after.Columns[1].Default = "2 expanded"
	after.Constraints[0].Def = "CHECK (x expanded)"
	after.Indexes[0].Def = "CREATE INDEX t_idx ON t USING btree (a) WHERE x expanded"
	settled := cloneReplayed(before)
	require.NoError(t, recordReplays(&settled, before, after))
	assert.Nil(t, settled.Columns[0].DefaultReplays)
	assert.Equal(t, []string{"2 expanded"}, settled.Columns[1].DefaultReplays)
	assert.Equal(t, []string{"CHECK (x expanded)"}, settled.Constraints[0].DefReplays)
	assert.Equal(t, []string{"CREATE INDEX t_idx ON t USING btree (a) WHERE x expanded"}, settled.Indexes[0].DefReplays)
	assert.Nil(t, before.Columns[1].DefaultReplays, "recording never writes through to the model it started from")
}

// The desired model takes a live text only when it is one of the replays
// of the same definition; any other live text is left for the diff to plan.
func TestAdoptReplayedText(t *testing.T) {
	desired := Model{
		Columns: []Column{
			{Name: "f", Default: "first", DefaultReplays: []string{"second"}},
			{Name: "g", Default: "first", DefaultReplays: []string{"second"}},
		},
		Constraints: []Constraint{
			{Name: "ck", Def: "CHECK (first)", DefReplays: []string{"CHECK (second)"}},
			{Name: "nv", Def: "CHECK (first)", DefReplays: []string{"CHECK (second)"}},
			{Name: "nv_other", Def: "CHECK (first)", DefReplays: []string{"CHECK (second)"}},
		},
		Indexes: []Index{
			{Name: "i", Def: "CREATE INDEX i ON t USING btree (a) WHERE first", DefReplays: []string{"CREATE INDEX i ON t USING btree (a) WHERE second"}},
		},
	}
	live := Model{
		Columns: []Column{{Name: "f", Default: "second"}, {Name: "g", Default: "other"}},
		Constraints: []Constraint{
			{Name: "ck", Def: "CHECK (second)"},
			{Name: "nv", Def: "CHECK (second) NOT VALID", NotValid: true},
			{Name: "nv_other", Def: "CHECK (other) NOT VALID", NotValid: true},
		},
		Indexes: []Index{{Name: "i", Def: "CREATE INDEX i ON t USING btree (a) WHERE second"}},
	}
	got := adoptReplayedText(live, desired)
	assert.Equal(t, "second", got.Columns[0].Default, "a replayed default is adopted")
	assert.Equal(t, "first", got.Columns[1].Default, "a different default is not")
	assert.Equal(t, "CHECK (second)", got.Constraints[0].Def, "a replayed check is adopted")
	assert.Equal(t, "CHECK (second)", got.Constraints[1].Def, "a NOT VALID replay is adopted without the suffix, so the diff plans the VALIDATE")
	assert.False(t, got.Constraints[1].NotValid)
	assert.Equal(t, "CHECK (first)", got.Constraints[2].Def, "a NOT VALID check that matches no replay is not adopted")
	assert.Equal(t, "CREATE INDEX i ON t USING btree (a) WHERE second", got.Indexes[0].Def, "a replayed index is adopted")
	assert.Equal(t, "first", desired.Columns[0].Default, "adopting never writes through to the desired model")
	assert.Equal(t, "CHECK (first)", desired.Constraints[0].Def)
}

// An index's live text is replayed only when it names the same index and
// table as the desired text, so a replay never resolves to another table.
func TestSameIndexTarget(t *testing.T) {
	desired := "CREATE INDEX i ON t USING btree (a) WHERE (a > 0)"
	assert.True(t, sameIndexTarget("CREATE INDEX i ON t USING btree (a) WHERE (a > 1)", desired))
	assert.False(t, sameIndexTarget("CREATE INDEX i ON app.t USING btree (a) WHERE (a > 0)", desired))
	assert.False(t, sameIndexTarget("CREATE UNIQUE INDEX i ON t USING btree (a) WHERE (a > 0)", desired))
	assert.False(t, sameIndexTarget("CREATE INDEX i ON t", "CREATE INDEX i ON t"), "a definition without an access method is never matched")
}

// The error for an expression that never settles names the definition and
// the last text it printed.
func TestUnsettledErrorNamesTheDefinition(t *testing.T) {
	before := Model{Table: "t", Constraints: []Constraint{{Name: "ck", Def: "CHECK (x)"}}}
	after := Model{Table: "t", Constraints: []Constraint{{Name: "ck", Def: "CHECK (x expanded)"}}}
	err := unsettledError(before, after)
	require.ErrorIs(t, err, ErrExpressionUnsettled)
	assert.EqualError(t, err, `the server prints a desired expression differently each time it parses it: constraint "ck" of table t still changed after 4 replays, to CHECK (x expanded); write it without nested row comparisons`)
}
