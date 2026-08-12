// White-box tests for sequence admission: the shape classification and
// fail-closed refusals that decide which budget class each step runs under
// — decisions that must be provable without a database.

package executor

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAdmitStepClassifiesShapes(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want StepKind
	}{
		{
			name: "ADD CONSTRAINT NOT VALID is brief",
			sql:  `ALTER TABLE s.t ADD CONSTRAINT c CHECK (v > 0) NOT VALID`,
			want: StepBrief,
		},
		{
			name: "a lone VALIDATE CONSTRAINT gets the validate class",
			sql:  `ALTER TABLE s.t VALIDATE CONSTRAINT c`,
			want: StepValidateConstraint,
		},
		{
			name: "SET NOT NULL is brief",
			sql:  `ALTER TABLE s.t ALTER COLUMN v SET NOT NULL`,
			want: StepBrief,
		},
		{
			name: "the scaffold DROP CONSTRAINT is brief",
			sql:  `ALTER TABLE s.t DROP CONSTRAINT c`,
			want: StepBrief,
		},
		{
			name: "a fast-default ADD COLUMN is brief",
			sql:  `ALTER TABLE s.t ADD COLUMN age int NOT NULL DEFAULT 0`,
			want: StepBrief,
		},
		{
			name: "a metadata-only change is brief",
			sql:  `ALTER TABLE s.t ALTER COLUMN v DROP DEFAULT`,
			want: StepBrief,
		},
		{
			name: "ADD CONSTRAINT USING INDEX is brief",
			sql:  `ALTER TABLE s.t ADD CONSTRAINT t_pkey PRIMARY KEY USING INDEX t_pkey`,
			want: StepBrief,
		},
		{
			name: "a concurrent index build is delegated",
			sql:  `CREATE UNIQUE INDEX CONCURRENTLY i ON s.t (v)`,
			want: StepConcurrentIndexBuild,
		},
		{
			name: "a VALIDATE sharing a multi-operation ALTER runs brief",
			sql:  `ALTER TABLE s.t VALIDATE CONSTRAINT c, ADD COLUMN x int`,
			want: StepBrief,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			step, err := admitStep("s", "t", tt.sql)
			require.NoError(t, err)
			assert.Equal(t, tt.want, step.kind)
		})
	}
}

func TestAdmitStepRefusals(t *testing.T) {
	tests := []struct {
		name    string
		sql     string
		wantErr error
	}{
		{
			name:    "a blocking CREATE INDEX never belongs in a sequence",
			sql:     `CREATE INDEX i ON s.t (v)`,
			wantErr: ErrUnsupportedSequenceStep,
		},
		{
			name:    "DROP INDEX CONCURRENTLY is not driven",
			sql:     `DROP INDEX CONCURRENTLY s.i`,
			wantErr: ErrUnsupportedSequenceStep,
		},
		{
			name:    "REINDEX CONCURRENTLY is not driven",
			sql:     `REINDEX INDEX CONCURRENTLY s.i`,
			wantErr: ErrUnsupportedSequenceStep,
		},
		{
			name:    "a cancelled DETACH PARTITION CONCURRENTLY leaves detach-pending state",
			sql:     `ALTER TABLE s.t DETACH PARTITION p CONCURRENTLY`,
			wantErr: ErrUnsupportedSequenceStep,
		},
		{
			name:    "CREATE TABLE is not a sequence step",
			sql:     `CREATE TABLE s.t (id int)`,
			wantErr: ErrUnsupportedSequenceStep,
		},
		{
			name:    "a step against another table breaks the preflight binding",
			sql:     `ALTER TABLE s.other DROP CONSTRAINT c`,
			wantErr: ErrInvariantViolation,
		},
		{
			name:    "a step against another schema breaks the preflight binding",
			sql:     `ALTER TABLE elsewhere.t DROP CONSTRAINT c`,
			wantErr: ErrInvariantViolation,
		},
		{
			name:    "an unqualified index build against a qualified preflight breaks the binding",
			sql:     `CREATE UNIQUE INDEX CONCURRENTLY i ON t (v)`,
			wantErr: ErrInvariantViolation,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := admitStep("s", "t", tt.sql)
			require.ErrorIs(t, err, tt.wantErr)
		})
	}
}

func TestAdmitSequenceNamesTheOffendingStep(t *testing.T) {
	steps := []string{
		`ALTER TABLE s.t ADD CONSTRAINT c CHECK (v > 0) NOT VALID`,
		`CREATE TABLE s.t (id int)`,
	}
	_, err := admitSequence("s", "t", steps)
	require.ErrorIs(t, err, ErrUnsupportedSequenceStep)
	var stepErr *SequenceStepError
	assert.False(t, errors.As(err, &stepErr), "an admission refusal is not a step failure: nothing executed")
}

func TestAdmitSequenceSurfacesParseFailures(t *testing.T) {
	_, err := admitSequence("s", "t", []string{`ALTER TABLE s.t THIS IS NOT SQL`})
	require.Error(t, err)
}
