package schemadiff

import (
	"strings"
	"testing"

	"github.com/block/pg-sprite/pkg/statement"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// checks returns base() with its constraints replaced by the given check
// constraints, each flagged as named by the server when generated is set.
func checks(generated bool, cons ...Constraint) Model {
	m := base()
	m.Constraints = []Constraint{{Name: "events_pkey", Def: "PRIMARY KEY (id)"}}
	for _, c := range cons {
		c.GeneratedName = generated
		m.Constraints = append(m.Constraints, c)
	}
	return m
}

const (
	checkA = "CHECK ((id > 0))"
	checkB = "CHECK ((length((name)::text) > 0))"
	checkC = "CHECK ((id < 1000000))"
)

// A table built by a history of ALTERs numbers its unnamed checks in the
// order they were added; the same table built from one file numbers them in
// file order. Only the names differ, so the plan is empty.
func TestDiffMatchesUnnamedChecksByDefinition(t *testing.T) {
	live := checks(false,
		Constraint{Name: "events_check", Def: checkA},
		Constraint{Name: "events_check1", Def: checkB},
		Constraint{Name: "events_check2", Def: checkC},
	)
	desired := checks(true,
		Constraint{Name: "events_check", Def: checkA},
		Constraint{Name: "events_check1", Def: checkC},
		Constraint{Name: "events_check2", Def: checkB},
	)
	changes, err := Diff("public", live, desired)
	require.NoError(t, err)
	assert.Empty(t, changes)
}

// Checks the file names keep the name-keyed diff: a reorder of named
// checks is a real rename, so it drops and re-adds under the stated names.
func TestDiffMatchesNamedChecksByName(t *testing.T) {
	live := checks(false,
		Constraint{Name: "events_check1", Def: checkB},
		Constraint{Name: "events_check2", Def: checkC},
	)
	desired := checks(false,
		Constraint{Name: "events_check1", Def: checkC},
		Constraint{Name: "events_check2", Def: checkB},
	)
	changes, err := Diff("public", live, desired)
	require.NoError(t, err)
	assert.Equal(t, []string{
		`ALTER TABLE "public"."events" DROP CONSTRAINT "events_check1"`,
		`ALTER TABLE "public"."events" DROP CONSTRAINT "events_check2"`,
		`ALTER TABLE "public"."events" ADD CONSTRAINT "events_check1" CHECK ((id < 1000000))`,
		`ALTER TABLE "public"."events" ADD CONSTRAINT "events_check2" CHECK ((length((name)::text) > 0))`,
	}, sqls(changes))
}

// One check changed alongside the reorder. The unchanged check keeps its
// live name; the new one would be added under the name that check now
// holds, so it moves on to the next free pass number instead of colliding.
func TestDiffAddsANewUnnamedCheckUnderAFreeName(t *testing.T) {
	live := checks(false,
		Constraint{Name: "events_check1", Def: checkA},
		Constraint{Name: "events_check2", Def: checkB},
	)
	desired := checks(true,
		Constraint{Name: "events_check1", Def: checkB},
		Constraint{Name: "events_check2", Def: checkC},
	)
	changes, err := Diff("public", live, desired)
	require.NoError(t, err)
	assert.Equal(t, []string{
		`ALTER TABLE "public"."events" DROP CONSTRAINT "events_check1"`,
		`ALTER TABLE "public"."events" ADD CONSTRAINT "events_check3" CHECK ((id < 1000000))`,
	}, sqls(changes))
	assert.Equal(t, []ChangeKind{ChangeDropConstraint, ChangeAddConstraint}, []ChangeKind{changes[0].Kind, changes[1].Kind})
	assert.True(t, changes[0].Destructive)
}

// A live check whose name the file states belongs to that declaration, so
// an unnamed check with the same definition never takes it over.
func TestDiffNeverMatchesAnUnnamedCheckToAStatedName(t *testing.T) {
	live := checks(false, Constraint{Name: "id_positive", Def: checkA})
	desired := checks(false, Constraint{Name: "id_positive", Def: checkC})
	desired.Constraints = append(desired.Constraints, Constraint{Name: "events_check", Def: checkA, GeneratedName: true})
	changes, err := Diff("public", live, desired)
	require.NoError(t, err)
	assert.Equal(t, []string{
		`ALTER TABLE "public"."events" DROP CONSTRAINT "id_positive"`,
		`ALTER TABLE "public"."events" ADD CONSTRAINT "id_positive" CHECK ((id < 1000000))`,
		`ALTER TABLE "public"."events" ADD CONSTRAINT "events_check" CHECK ((id > 0))`,
	}, sqls(changes))
}

// A live check an interrupted online add left NOT VALID, under a shifted
// name, is validated under its live name rather than dropped and re-added.
func TestDiffValidatesAnUnnamedCheckUnderItsLiveName(t *testing.T) {
	live := checks(false,
		Constraint{Name: "events_check", Def: checkA},
		Constraint{Name: "events_check1", Def: checkB + " NOT VALID", NotValid: true},
	)
	desired := checks(true,
		Constraint{Name: "events_check", Def: checkB},
		Constraint{Name: "events_check1", Def: checkA},
	)
	changes, err := Diff("public", live, desired)
	require.NoError(t, err)
	assert.Equal(t, []string{`ALTER TABLE "public"."events" VALIDATE CONSTRAINT "events_check1"`}, sqls(changes))
	require.Len(t, changes, 1)
	require.NotNil(t, changes[0].Validates)
	assert.Equal(t, "events_check1", changes[0].Validates.Constraint)
}

// Each live check matches at most one desired check: a second desired copy
// of the same definition is added rather than matched twice.
func TestDiffMatchesEachLiveCheckOnce(t *testing.T) {
	live := checks(false, Constraint{Name: "events_check", Def: checkA})
	desired := checks(true,
		Constraint{Name: "events_check", Def: checkA},
		Constraint{Name: "events_check1", Def: checkA},
	)
	changes, err := Diff("public", live, desired)
	require.NoError(t, err)
	assert.Equal(t, []string{`ALTER TABLE "public"."events" ADD CONSTRAINT "events_check1" CHECK ((id > 0))`}, sqls(changes))
}

func TestNextGeneratedCheckName(t *testing.T) {
	name, err := nextGeneratedCheckName("events_check2", map[string]bool{"events_check1": true, "events_check2": true})
	require.NoError(t, err)
	assert.Equal(t, "events_check3", name)

	name, err = nextGeneratedCheckName("events_qty_check", map[string]bool{"events_qty_check": true})
	require.NoError(t, err)
	assert.Equal(t, "events_qty_check1", name)

	// The server shortens the table and column parts to fit a longer pass
	// number; the numbering here does not, so it refuses.
	full := strings.Repeat("a", maxIdentifierBytes-len("_check")) + "_check"
	_, err = nextGeneratedCheckName(full, map[string]bool{full: true})
	require.ErrorIs(t, err, ErrUnsupportedChange)

	_, err = nextGeneratedCheckName("events_positive", nil)
	require.ErrorIs(t, err, ErrUnsupportedChange)
}

// An unnamed desired check that some live check already prints needs no
// replay, so none of the other live checks is tried as its precursor. A
// check whose live text moved still gets every live constraint the file
// does not name.
func TestLivePrecursorCandidatesStopAtAnExactMatch(t *testing.T) {
	live := checks(false,
		Constraint{Name: "events_check", Def: checkA},
		Constraint{Name: "events_check1", Def: checkB},
	)
	desired := checks(true,
		Constraint{Name: "events_check", Def: checkB},
		Constraint{Name: "events_check1", Def: "CHECK ((id > 1))"},
	)
	matched := desired.Constraints[1]
	assert.Empty(t, livePrecursorCandidates(live, desired, matched))

	moved := desired.Constraints[2]
	var names []string
	for _, c := range livePrecursorCandidates(live, desired, moved) {
		names = append(names, c.Name)
	}
	assert.Equal(t, []string{"events_check", "events_check1"}, names)
}

// Two unnamed desired checks with the same text need two live copies. When
// only one live check prints that text and another prints it one replay
// behind, the exact copy can match just one of them, so both still get
// every live constraint the file does not name.
func TestLivePrecursorCandidatesCountDuplicateChecks(t *testing.T) {
	const older = "CHECK (((id) > 0))"
	live := checks(false,
		Constraint{Name: "events_check", Def: checkA},
		Constraint{Name: "events_check1", Def: older},
	)
	desired := checks(true,
		Constraint{Name: "events_check", Def: checkA},
		Constraint{Name: "events_check1", Def: checkA},
	)
	for _, c := range desired.Constraints[1:] {
		var names []string
		for _, l := range livePrecursorCandidates(live, desired, c) {
			names = append(names, l.Name)
		}
		assert.Equal(t, []string{"events_check", "events_check1"}, names, c.Name)
	}

	live.Constraints[2].Def = checkA
	for _, c := range desired.Constraints[1:] {
		assert.Empty(t, livePrecursorCandidates(live, desired, c), c.Name)
	}
}

// A LIKE clause that copies constraints can bring in checks under names the
// file does not state, so no check is flagged as server-named and every
// check keeps matching by name. Without such a clause, the unnamed check is
// flagged.
func TestMarkGeneratedCheckNamesKeepsNameMatchingUnderLikeIncludingConstraints(t *testing.T) {
	cases := []struct {
		name      string
		sql       string
		generated bool
	}{
		{name: "including constraints", sql: "CREATE TABLE events (LIKE template INCLUDING CONSTRAINTS, id int CHECK (id > 0));"},
		{name: "including all", sql: "CREATE TABLE events (LIKE template INCLUDING ALL, id int CHECK (id > 0));"},
		{name: "including defaults only", sql: "CREATE TABLE events (LIKE template INCLUDING DEFAULTS, id int CHECK (id > 0));", generated: true},
		{name: "no like clause", sql: "CREATE TABLE events (id int CHECK (id > 0));", generated: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ds, err := statement.ParseDesired(tc.sql)
			require.NoError(t, err)
			m := checks(false, Constraint{Name: "events_id_check", Def: checkA})
			require.NoError(t, markGeneratedCheckNames(ds.Statements(), &m))
			assert.False(t, m.Constraints[0].GeneratedName, "the primary key is never a check")
			assert.Equal(t, tc.generated, m.Constraints[1].GeneratedName)
		})
	}
}
