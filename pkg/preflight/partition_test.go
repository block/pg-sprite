package preflight

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestRefusesPartitionedParentDistinguishesIndexBuilds(t *testing.T) {
	cause, err := RefusesPartitionedParent(16, []string{"CREATE INDEX CONCURRENTLY i ON s.t (id)"})
	require.NoError(t, err)
	assert.Equal(t, PartitionCauseConcurrentIndexBuild, cause)

	cause, err = RefusesPartitionedParent(16, []string{"CREATE INDEX i ON s.t (id)"})
	require.NoError(t, err)
	assert.Equal(t, PartitionCauseBlockingIndexBuild, cause)
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
