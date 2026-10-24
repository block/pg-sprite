package schemadiff

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A valid live table produces no advisory: advisories exist only for the
// observations the diff declines to act on.
func TestAdvisoriesNoneForValidIndexes(t *testing.T) {
	assert.Empty(t, Advisories(base(), base()))
}

// The invalid entry desired names with the same definition is the rebuild
// case: the diff plans the create, and the advisory says the create
// completes only through the proven-removal path.
func TestAdvisoriesInvalidIndexNamedByDesiredIsRebuild(t *testing.T) {
	live := base()
	live.Indexes[0].Invalid = true
	assert.Equal(t, []Advisory{
		{Kind: AdvisoryInvalidIndexRebuild, Index: "events_name_idx"},
	}, Advisories(live, base()))
}

// A redefined invalid entry is still the rebuild case: desired names the
// index, so the create is planned and the same completion path applies.
func TestAdvisoriesInvalidIndexRedefinedIsRebuild(t *testing.T) {
	live := base()
	live.Indexes[0].Invalid = true
	desired := base()
	desired.Indexes[0].Def = "CREATE UNIQUE INDEX events_name_idx ON events USING btree (name)"
	assert.Equal(t, []Advisory{
		{Kind: AdvisoryInvalidIndexRebuild, Index: "events_name_idx"},
	}, Advisories(live, desired))
}

// An invalid entry desired no longer names yields no change and one
// undeclared advisory: the observation the change list would discard. A
// valid entry in the same position is dropped as usual and produces no
// advisory.
func TestAdvisoriesInvalidIndexAbsentFromDesiredIsUndeclared(t *testing.T) {
	live := base()
	live.Indexes = append(live.Indexes,
		Index{Name: "events_name_idx_ccnew", Def: "CREATE INDEX events_name_idx_ccnew ON events USING btree (name)", Invalid: true},
		Index{Name: "events_legacy_idx", Def: "CREATE INDEX events_legacy_idx ON events USING btree (id)"},
	)
	desired := base()
	desired.Indexes = nil

	changes, err := Diff("public", live, desired)
	require.NoError(t, err)
	assert.Equal(t, []string{
		`DROP INDEX "public"."events_name_idx"`,
		`DROP INDEX "public"."events_legacy_idx"`,
	}, sqls(changes), "the valid entries drop; the invalid one is left alone")
	assert.Equal(t, []Advisory{
		{Kind: AdvisoryInvalidIndexUndeclared, Index: "events_name_idx_ccnew"},
	}, Advisories(live, desired))
}

// Advisories follow live index order and classify each entry independently.
func TestAdvisoriesReportEveryInvalidEntryInLiveOrder(t *testing.T) {
	live := base()
	live.Indexes[0].Invalid = true
	live.Indexes = append(live.Indexes,
		Index{Name: "events_name_idx_ccnew", Def: "CREATE INDEX events_name_idx_ccnew ON events USING btree (name)", Invalid: true},
	)
	assert.Equal(t, []Advisory{
		{Kind: AdvisoryInvalidIndexRebuild, Index: "events_name_idx"},
		{Kind: AdvisoryInvalidIndexUndeclared, Index: "events_name_idx_ccnew"},
	}, Advisories(live, base()))
}

// On a partitioned parent an invalid index means unattached partition
// indexes, not an unfinished build, so it is never advised — whether or
// not desired names it.
func TestAdvisoriesNoneOnPartitionedParent(t *testing.T) {
	live := base()
	live.PartitionKey = "RANGE (id)"
	live.Indexes[0].Invalid = true
	desired := base()
	desired.PartitionKey = "RANGE (id)"
	assert.Empty(t, Advisories(live, desired))

	desired.Indexes = nil
	assert.Empty(t, Advisories(live, desired))
}

// The kind set is a closed vocabulary a consumer branches on; pinned to
// the plan report's format_version.
func TestAdvisoryKindsVocabularyPinned(t *testing.T) {
	assert.Equal(t, []AdvisoryKind{
		AdvisoryInvalidIndexRebuild,
		AdvisoryInvalidIndexUndeclared,
	}, AdvisoryKinds())
}

// Every kind describes itself; an unknown kind falls back to its own token
// so a renderer never prints an empty note.
func TestAdvisoryKindDescriptions(t *testing.T) {
	for _, k := range AdvisoryKinds() {
		assert.NotEqual(t, string(k), k.Description(), "kind %q has no description", k)
	}
	assert.Equal(t, "no-such-kind", AdvisoryKind("no-such-kind").Description())
}
