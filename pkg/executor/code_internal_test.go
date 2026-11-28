package executor

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The online attach's steps stop on sentinels of their own, unexported, so
// an adapter learns their cause only from the code. Each is wrapped here the
// way its step wraps it.
func TestOutcomeCodeMapsTheOnlineAttachSteps(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want Code
	}{
		{name: "bound unprovable", err: fmt.Errorf("%w: the key has a composite type", errAttachBoundUnprovable), want: CodeAttachPartitionBoundUnprovable},
		{name: "proof name taken", err: fmt.Errorf("give app.t_2027 the parent's CHECK c: %w", errAttachProofNameTaken), want: CodeAttachPartitionProofNameTaken},
		{name: "rows outside the bound", err: fmt.Errorf("validate the proof on app.t_2027: %w", errAttachProofViolation), want: CodeDataFault},
		{name: "column missing", err: fmt.Errorf("make column \"c\" of app.t_2027 NOT NULL: %w", errAttachColumnMissing), want: CodeAttachPartitionColumnMissing},
		{name: "not-null NOT VALID", err: fmt.Errorf("make column \"c\" of app.t_2027 NOT NULL: %w", errAttachNotNullNotValid), want: CodeAttachPartitionNotNullNotValid},
		{name: "composite column", err: fmt.Errorf("make column \"c\" of app.t_2027 NOT NULL: %w", errAttachColumnRowType), want: CodeAttachPartitionColumnRowType},
		{name: "index unsupported", err: fmt.Errorf("give app.t_2027 the index of t_excl: %w", errAttachIndexUnsupported), want: CodeAttachPartitionIndexUnsupported},
		{name: "foreign key unsupported", err: fmt.Errorf("give app.t_2027 the foreign key t_fk: %w", errAttachForeignKeyUnsupported), want: CodeAttachPartitionForeignKeyUnsupported},
		{name: "attached under another bound", err: fmt.Errorf("attach app.t_2027: %w", errAttachedUnderAnotherBound), want: CodeAttachPartitionUnderAnotherBound},
		{name: "parent changed", err: fmt.Errorf("%w: rerun", errAttachParentChanged), want: CodeAttachPartitionParentChanged},
		{name: "proof not ready", err: fmt.Errorf("attach app.t_2027: %w", errAttachProofNotReady), want: CodeAttachPartitionNotReady},
		{name: "DEFAULT partition changed", err: fmt.Errorf("attach app.t_2027: %w", errAttachDefaultPartitionChanged), want: CodeAttachPartitionNotReady},
		{name: "proofs not of the bound", err: fmt.Errorf("attach app.t_2027: %w", errAttachProofsNotOfTheBound), want: CodeAttachPartitionNotReady},
		{name: "index not ready", err: fmt.Errorf("attach app.t_2027: %w", errAttachIndexNotReady), want: CodeAttachPartitionNotReady},
		{name: "foreign key not ready", err: fmt.Errorf("attach app.t_2027: %w", errAttachForeignKeyNotReady), want: CodeAttachPartitionNotReady},
		{name: "index incomplete", err: fmt.Errorf("%w: the parent's indexes changed", errAttachIndexIncomplete), want: CodeAttachPartitionNotReady},
		// The foreign key step runs the partitioned foreign key build's leaf
		// step, whose refusals keep their own codes under the step's wrap.
		{name: "leaf foreign key name taken", err: fmt.Errorf("give app.t_2027 the foreign key t_fk: %w", ErrPartitionedForeignKeyNameTaken), want: CodePartitionedForeignKeyNameTaken},
		// A lost lock voids the step whatever it stopped on.
		{name: "invariant outranks the step", err: fmt.Errorf("%w: %w", ErrInvariantViolation, errAttachProofNotReady), want: CodeInvariantViolation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, OutcomeCode(tt.err))
		})
	}
}
