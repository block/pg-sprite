package preflight

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/refusal"
)

func TestCheckPartitionSupport(t *testing.T) {
	plain := PreflightedTable{relkind: "r"}
	require.NoError(t, CheckPartitionSupport(plain, 16, []string{"CREATE INDEX i ON s.t (id)"}))

	parent := PreflightedTable{relkind: "p"}
	err := CheckPartitionSupport(parent, 16, []string{"CREATE INDEX i ON s.t (id)"})
	var unsupported *UnsupportedPartitionedParentError
	require.ErrorAs(t, err, &unsupported)
	assert.Equal(t, PartitionCauseBlockingIndexBuild, unsupported.Cause)

	err = CheckPartitionSupport(parent, 16, []string{"ALTER TABLE s.t THIS IS NOT SQL"})
	require.Error(t, err)
	assert.False(t, errors.As(err, &unsupported), "parse failures are operational errors, not refusals")
}

// The index-build refusals are raised for any statement that builds an index
// on a parent, including a constraint-building ALTER TABLE a forced run
// submits as written, which the partitioned build does not take. Their
// messages therefore name the statement the build does take, a CREATE INDEX,
// and never promise the same alternative for the request in hand.
func TestIndexBuildRefusalNamesOnlyTheStatementTheBuildTakes(t *testing.T) {
	for _, cause := range []PartitionRefusalCause{PartitionCauseBlockingIndexBuild, PartitionCauseConcurrentIndexBuild} {
		msg := (&UnsupportedPartitionedParentError{Cause: cause}).Error()
		assert.Contains(t, msg, "a CREATE INDEX on a partitioned table", string(cause))
		assert.Contains(t, msg, "partition by partition instead", string(cause))
		assert.NotContains(t, msg, "pg-sprite builds the index", string(cause),
			"the message must not promise the build for whatever statement was refused")
	}
}

func TestRefusesPartitionedParentDistinguishesIndexBuilds(t *testing.T) {
	cause, err := RefusesPartitionedParent(16, []string{"CREATE INDEX CONCURRENTLY i ON s.t (id)"})
	require.NoError(t, err)
	assert.Equal(t, PartitionCauseConcurrentIndexBuild, cause)

	cause, err = RefusesPartitionedParent(16, []string{"CREATE INDEX i ON s.t (id)"})
	require.NoError(t, err)
	assert.Equal(t, PartitionCauseBlockingIndexBuild, cause)
}

// Exactly one CREATE INDEX, in any form, is the shape the partitioned index
// build carries; no other step list is, including a CREATE INDEX that has
// company, and a step that does not parse is an error, not a refusal.
func TestRoutesToPartitionedIndexBuild(t *testing.T) {
	for _, sql := range []string{
		"CREATE INDEX i ON s.t (id)",
		"CREATE INDEX CONCURRENTLY i ON s.t (id)",
		"CREATE UNIQUE INDEX i ON ONLY s.t (id)",
		"CREATE INDEX IF NOT EXISTS i ON s.t USING btree (id)",
	} {
		routes, err := RoutesToPartitionedIndexBuild([]string{sql})
		require.NoError(t, err, sql)
		assert.True(t, routes, sql)
	}

	for name, steps := range map[string][]string{
		"no steps":            nil,
		"an ALTER":            {"ALTER TABLE s.t ADD COLUMN c int"},
		"an index adoption":   {"ALTER TABLE s.t ADD CONSTRAINT t_pk PRIMARY KEY USING INDEX ix"},
		"a unique constraint": {"ALTER TABLE s.t ADD CONSTRAINT t_u UNIQUE (id)"},
		"an index with company": {
			"CREATE INDEX i ON s.t (id)",
			"ALTER TABLE s.t ADD COLUMN c int",
		},
		"a sequence of two indexes": {"CREATE INDEX i ON s.t (id)", "CREATE INDEX j ON s.t (c)"},
	} {
		routes, err := RoutesToPartitionedIndexBuild(steps)
		require.NoError(t, err, name)
		assert.False(t, routes, name)
	}

	_, err := RoutesToPartitionedIndexBuild([]string{"CREATE INDEX"})
	require.Error(t, err)
}

// RF-6: the routing is only an exemption from refusal for the front doors
// that run the partitioned index build. Every step list the routing names is
// still refused by the static policy, so a path that runs the steps instead
// of the build (a forced run, a library caller driving steps directly) is
// refused before its first step, never run as a blocking parent build.
func TestEveryRoutedPartitionedIndexBuildShapeIsRefusedWhenItBypassesTheBuild(t *testing.T) {
	for _, sql := range []string{
		"CREATE INDEX i ON s.t (id)",
		"CREATE UNIQUE INDEX i ON s.t (id)",
		"CREATE INDEX CONCURRENTLY i ON s.t (id)",
		"CREATE INDEX i ON ONLY s.t (id)",
		"CREATE INDEX IF NOT EXISTS i ON s.t (id)",
		"CREATE INDEX ON s.t (id)",
	} {
		routes, err := RoutesToPartitionedIndexBuild([]string{sql})
		require.NoError(t, err, sql)
		require.True(t, routes, sql)
		for _, major := range []int{14, 18} {
			cause, err := RefusesPartitionedParent(major, []string{sql})
			require.NoError(t, err, sql)
			assert.Contains(t, []PartitionRefusalCause{PartitionCauseBlockingIndexBuild, PartitionCauseConcurrentIndexBuild}, cause,
				"%s on version %d must be refused as an index build when it bypasses the build", sql, major)
		}
	}
}

func TestRefusesPartitionedParentAdoptsExistingIndex(t *testing.T) {
	for _, major := range []int{14, 15, 16, 17, 18} {
		cause, err := RefusesPartitionedParent(major,
			[]string{`ALTER TABLE public.p ADD CONSTRAINT p_pk PRIMARY KEY USING INDEX ix_p_id`})
		require.NoError(t, err)
		require.Equal(t, PartitionCauseIndexAdoption, cause)
	}
}

func TestRefusesPartitionedParentNotValidForeignKeyByServerVersion(t *testing.T) {
	steps := []string{"ALTER TABLE s.t ADD CONSTRAINT t_fk FOREIGN KEY (parent_id) REFERENCES s.parent(id) NOT VALID"}
	cause, err := RefusesPartitionedParent(17, steps)
	require.NoError(t, err)
	assert.Equal(t, PartitionCauseNotValidForeignKey, cause)
	cause, err = RefusesPartitionedParent(18, steps)
	require.NoError(t, err)
	assert.Empty(t, cause)

	checkSteps := []string{"ALTER TABLE s.t ADD CONSTRAINT t_check CHECK (parent_id > 0) NOT VALID"}
	cause, err = RefusesPartitionedParent(14, checkSteps)
	require.NoError(t, err)
	assert.Empty(t, cause)
}

// Every partition cause routes to the class docs/refusal-classes.md decides
// for it: the two index-build causes are a capability boundary, index
// adoption is by design, the NOT VALID foreign key is environmental; every
// answer is a member of refusal.Classes(), and the zero cause — the steps
// are supported — has no class.
func TestEveryPartitionRefusalCauseNamesItsClass(t *testing.T) {
	want := map[PartitionRefusalCause]refusal.Class{
		PartitionCauseConcurrentIndexBuild: refusal.ClassCapabilityBoundary,
		PartitionCauseBlockingIndexBuild:   refusal.ClassCapabilityBoundary,
		PartitionCauseIndexAdoption:        refusal.ClassByDesign,
		PartitionCauseNotValidForeignKey:   refusal.ClassEnvironmental,
	}
	assert.Len(t, PartitionRefusalCauses(), len(want), "every registered cause has an expected class")
	for _, cause := range PartitionRefusalCauses() {
		assert.Equal(t, want[cause], cause.Class(), "cause %q", cause)
		assert.Contains(t, refusal.Classes(), cause.Class(), "cause %q answers a class refusal.Classes() does not list", cause)
	}
	assert.Empty(t, PartitionRefusalCause("").Class())
	assert.Empty(t, PartitionRefusalCause("not-a-cause").Class())
}
