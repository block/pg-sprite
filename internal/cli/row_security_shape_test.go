package cli

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/verdict"
)

// migrate --desired refuses to change the row security of an unlogged table
// with the same plain text diff prints: what the table is, and what to do
// instead, without the executor's internal error chain.
func TestMigrateRowSecurityShapeRefusalSaysWhatToDo(t *testing.T) {
	shape := executor.RowSecurityTableShapeRefusal(schemadiff.Model{Table: "audit_log", Unlogged: true})
	assert.ErrorIs(t, shape, executor.ErrRowSecurityTableShape)

	// The executor wraps the shape refusal in its admission error.
	err := fmt.Errorf("admit row security target public.audit_log: %w: %w", executor.ErrRowSecurityRefused, shape)
	v := rowSecurityVerdict("public.audit_log", executor.RowSecurityReport{}, err)

	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, "migrate --desired does not change row security on this table because it is unlogged; "+
		"change its row security outside pg-sprite", v.Detail)
}

// A renderer verdict with no listed reason still prints its own text.
func TestRowSecurityShapeReasonFallsBackToTheVerdict(t *testing.T) {
	assert.Equal(t, "it is a partition", rowSecurityShapeReason(schemadiff.ErrUnrenderablePartition))
	assert.Equal(t, schemadiff.ErrUnrenderableRowSecurity.Error(), rowSecurityShapeReason(schemadiff.ErrUnrenderableRowSecurity))
}
