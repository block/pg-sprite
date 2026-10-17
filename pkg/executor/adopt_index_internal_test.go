package executor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// The drift check runs only inside a brief native attempt, so a planned
// adoption that a step of any other kind would perform is a breach of the
// plan's contract and refuses before anything runs. Only the brief step
// that takes the index over carries the adoption.
func TestAdoptionStepRequiresABriefStep(t *testing.T) {
	adopt, err := statement.ParseOne(`ALTER TABLE s.t ADD CONSTRAINT t_email_key UNIQUE USING INDEX t_email_key`)
	require.NoError(t, err)
	other, err := statement.ParseOne(`ALTER TABLE s.t ADD COLUMN note text`)
	require.NoError(t, err)
	adopts := &schemadiff.IndexAdoption{Index: "t_email_key", Def: "CREATE UNIQUE INDEX t_email_key ON t USING btree (email)"}

	for _, kind := range []StepKind{StepConcurrentIndexBuild, StepValidateConstraint} {
		t.Run(string(kind), func(t *testing.T) {
			_, err := adoptionStep([]sequenceStep{{st: other, kind: StepBrief}, {st: adopt, kind: kind}}, adopts)
			require.ErrorIs(t, err, ErrInvariantViolation)
			assert.Contains(t, err.Error(), "step 2 takes over index t_email_key as a "+string(kind)+" step")
		})
	}

	i, err := adoptionStep([]sequenceStep{{st: other, kind: StepBrief}, {st: adopt, kind: StepBrief}}, adopts)
	require.NoError(t, err)
	assert.Equal(t, 1, i)
}
