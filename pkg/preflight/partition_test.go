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
	assert.Equal(t, PartitionCauseIndexBuild, unsupported.Cause)

	err = CheckPartitionSupport(parent, 16, []string{"ALTER TABLE s.t THIS IS NOT SQL"})
	require.Error(t, err)
	assert.False(t, errors.As(err, &unsupported), "parse failures are operational errors, not refusals")
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
