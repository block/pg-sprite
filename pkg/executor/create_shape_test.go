package executor_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/statement"
)

func TestCreateShapeRefusalCauses(t *testing.T) {
	tests := []struct {
		name     string
		sql      string
		position int
		cause    executor.CreateShapeCause
		sentinel error
		code     executor.Code
	}{
		{name: "partition of", sql: "CREATE TABLE t PARTITION OF parent FOR VALUES FROM (1) TO (2)", cause: executor.CreateShapePartitionOf, sentinel: executor.ErrPartitionOfUnsupported, code: executor.CodePartitionOfUnsupported},
		{name: "inherits", sql: "CREATE TABLE t (id int) INHERITS (parent)", cause: executor.CreateShapeInherits, sentinel: executor.ErrUnsupportedCreateStep, code: executor.CodeUnsupportedCreateStep},
		{name: "like", sql: "CREATE TABLE t (LIKE source)", cause: executor.CreateShapeLike, sentinel: executor.ErrUnsupportedCreateStep, code: executor.CodeUnsupportedCreateStep},
		{name: "of type", sql: "CREATE TABLE t OF source_type", cause: executor.CreateShapeOfType, sentinel: executor.ErrUnsupportedCreateStep, code: executor.CodeUnsupportedCreateStep},
		{name: "table if not exists", sql: "CREATE TABLE IF NOT EXISTS t (id int)", cause: executor.CreateShapeIfNotExists, sentinel: executor.ErrIfNotExistsUnsupported, code: executor.CodeIfNotExistsUnsupported},
		{name: "index if not exists", sql: "CREATE TABLE t (id int); CREATE INDEX IF NOT EXISTS t_id ON t (id)", position: 1, cause: executor.CreateShapeIfNotExists, sentinel: executor.ErrIfNotExistsUnsupported, code: executor.CodeIfNotExistsUnsupported},
		{name: "duplicate name", sql: "CREATE TABLE t (id int PRIMARY KEY); CREATE INDEX t_pkey ON t (id)", position: 1, cause: executor.CreateShapeDuplicateName, sentinel: executor.ErrDuplicateCreateName, code: executor.CodeDuplicateCreateName},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ds, err := statement.ParseDesired(tc.sql)
			require.NoError(t, err)
			refusals, err := executor.CreateShapeRefusals("app", ds)
			require.NoError(t, err)
			require.Error(t, refusals[tc.position])
			assert.Equal(t, tc.cause, executor.CreateShapeCauseOf(refusals[tc.position]))
			assert.ErrorIs(t, refusals[tc.position], tc.sentinel)
			assert.Equal(t, tc.code, executor.OutcomeCode(refusals[tc.position]))
		})
	}
}

func TestCreateShapeErrorMappings(t *testing.T) {
	tests := []struct {
		cause    executor.CreateShapeCause
		sentinel error
		code     executor.Code
	}{
		{executor.CreateShapePartitionOf, executor.ErrPartitionOfUnsupported, executor.CodePartitionOfUnsupported},
		{executor.CreateShapeInherits, executor.ErrUnsupportedCreateStep, executor.CodeUnsupportedCreateStep},
		{executor.CreateShapeLike, executor.ErrUnsupportedCreateStep, executor.CodeUnsupportedCreateStep},
		{executor.CreateShapeOfType, executor.ErrUnsupportedCreateStep, executor.CodeUnsupportedCreateStep},
		{executor.CreateShapeIfNotExists, executor.ErrIfNotExistsUnsupported, executor.CodeIfNotExistsUnsupported},
		{executor.CreateShapeConcurrently, executor.ErrUnsupportedCreateStep, executor.CodeUnsupportedCreateStep},
		{executor.CreateShapeDuplicateName, executor.ErrDuplicateCreateName, executor.CodeDuplicateCreateName},
		{executor.CreateShapeMultipleOperations, executor.ErrUnsupportedCreateStep, executor.CodeUnsupportedCreateStep},
		{executor.CreateShapeUnsupportedKind, executor.ErrUnsupportedCreateStep, executor.CodeUnsupportedCreateStep},
	}
	for _, tc := range tests {
		t.Run(string(tc.cause), func(t *testing.T) {
			err := &executor.CreateShapeError{Cause: tc.cause}
			assert.ErrorIs(t, err, tc.sentinel)
			assert.Equal(t, tc.code, executor.OutcomeCode(err))
		})
	}
}

func TestCreateShapeCauseOf(t *testing.T) {
	assert.Empty(t, executor.CreateShapeCauseOf(nil))
	assert.Empty(t, executor.CreateShapeCauseOf(errors.New("unrelated")))

	shapeErr := &executor.CreateShapeError{Cause: executor.CreateShapeLike}
	stepErr := &executor.SequenceStepError{Step: 1, Total: 1, Err: shapeErr}
	assert.Equal(t, executor.CreateShapeLike, executor.CreateShapeCauseOf(stepErr))
}

func TestCreateShapeCauseDescriptionsAreDistinct(t *testing.T) {
	seen := make(map[string]executor.CreateShapeCause)
	for _, cause := range executor.CreateShapeCauses() {
		description := cause.Description()
		assert.NotEmpty(t, description)
		if previous, exists := seen[description]; exists {
			assert.Fail(t, "duplicate description", "%q and %q share %q", previous, cause, description)
		}
		seen[description] = cause
	}
	assert.NotEmpty(t, executor.CreateShapeCause("unknown").Description())
}
