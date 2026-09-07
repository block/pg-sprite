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

// The duplicate-name refusal names the colliding relation, so an operator
// reading the CLI sees which name the desired set claimed twice.
func TestCreateShapeRefusalNamesDuplicate(t *testing.T) {
	ds, err := statement.ParseDesired("CREATE TABLE t (id int PRIMARY KEY); CREATE INDEX t_pkey ON t (id)")
	require.NoError(t, err)
	refusals, err := executor.CreateShapeRefusals("app", ds)
	require.NoError(t, err)
	require.Error(t, refusals[1])
	assert.Equal(t, executor.CreateShapeDuplicateName.Description()+`: "t_pkey"`, refusals[1].Error())
}

// Every published cause maps to a sentinel, an outcome code, and its own
// sentence. The table is checked against CreateShapeCauses() in both
// directions, so a cause added to the vocabulary without a mapping — or a
// Description or Unwrap arm left to fall through to the default — fails here
// rather than surfacing as "unknown create-shape refusal" with the
// execution-failed code at runtime.
func TestCreateShapeErrorMappings(t *testing.T) {
	// keyword is the fragment that identifies the cause's own sentence, so
	// two causes cannot silently swap prose.
	tests := map[executor.CreateShapeCause]struct {
		sentinel error
		code     executor.Code
		keyword  string
	}{
		executor.CreateShapePartitionOf:        {executor.ErrPartitionOfUnsupported, executor.CodePartitionOfUnsupported, "PARTITION OF"},
		executor.CreateShapeInherits:           {executor.ErrUnsupportedCreateStep, executor.CodeUnsupportedCreateStep, "INHERITS"},
		executor.CreateShapeLike:               {executor.ErrUnsupportedCreateStep, executor.CodeUnsupportedCreateStep, "LIKE"},
		executor.CreateShapeOfType:             {executor.ErrUnsupportedCreateStep, executor.CodeUnsupportedCreateStep, "OF binds"},
		executor.CreateShapeIfNotExists:        {executor.ErrIfNotExistsUnsupported, executor.CodeIfNotExistsUnsupported, "IF NOT EXISTS"},
		executor.CreateShapeConcurrently:       {executor.ErrUnsupportedCreateStep, executor.CodeUnsupportedCreateStep, "concurrent build"},
		executor.CreateShapeDuplicateName:      {executor.ErrDuplicateCreateName, executor.CodeDuplicateCreateName, "same relation name twice"},
		executor.CreateShapeMultipleOperations: {executor.ErrUnsupportedCreateStep, executor.CodeUnsupportedCreateStep, "multiple operations"},
		executor.CreateShapeUnsupportedKind:    {executor.ErrUnsupportedCreateStep, executor.CodeUnsupportedCreateStep, "statement kind"},
	}
	causes := executor.CreateShapeCauses()
	require.Len(t, tests, len(causes), "every cause in CreateShapeCauses() needs a mapping row")

	unknown := executor.CreateShapeCause("unknown").Description()
	for _, cause := range causes {
		t.Run(string(cause), func(t *testing.T) {
			tc, mapped := tests[cause]
			require.True(t, mapped, "cause %q has no mapping row", cause)

			err := &executor.CreateShapeError{Cause: cause}
			require.NotNil(t, err.Unwrap(), "Unwrap falls through to the default arm")
			assert.ErrorIs(t, err, tc.sentinel)
			assert.Equal(t, tc.code, executor.OutcomeCode(err))
			assert.Equal(t, cause, executor.CreateShapeCauseOf(err))

			description := cause.Description()
			assert.NotEqual(t, unknown, description, "Description falls through to the default arm")
			assert.Contains(t, description, tc.keyword)
			assert.Equal(t, description, err.Error(), "a refusal without a name renders exactly its sentence")
		})
	}
}

// Only the duplicate-name refusal carries a relation name; the name is
// rendered after the sentence, and other causes ignore a stray name.
func TestCreateShapeErrorRendersName(t *testing.T) {
	named := &executor.CreateShapeError{Cause: executor.CreateShapeDuplicateName, Name: "t_pkey"}
	assert.Equal(t, executor.CreateShapeDuplicateName.Description()+`: "t_pkey"`, named.Error())

	unnamed := &executor.CreateShapeError{Cause: executor.CreateShapeDuplicateName}
	assert.Equal(t, executor.CreateShapeDuplicateName.Description(), unnamed.Error())

	other := &executor.CreateShapeError{Cause: executor.CreateShapeLike, Name: "t_pkey"}
	assert.Equal(t, executor.CreateShapeLike.Description(), other.Error())
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
