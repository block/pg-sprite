package applier

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/block/pg-sprite/pkg/decode"
)

// inShadowForm marks each value as read from the shadow. A column with no
// value has nothing to mark: it stays absent, so binding it is still
// refused. Wrapping it as a present nil would write a NULL over the
// stored value (CO-8). A NULL stays present and binds nil.
func TestInShadowFormCarriesAnAbsentColumnAsAbsent(t *testing.T) {
	out := inShadowForm([]decode.Column{decode.AbsentColumn("blob"), decode.PresentColumn("note", nil)})

	assert.False(t, out[0].Present(), "a column with no value stays without one")
	assert.Equal(t, "blob", out[0].Name())
	_, err := bindValue(out[0])
	assert.ErrorIs(t, err, ErrInvariantViolation)

	value, present := out[1].Get()
	assert.True(t, present, "a NULL stays present")
	assert.Equal(t, shadowForm{value: nil}, value)
	bound, err := bindValue(out[1])
	assert.NoError(t, err)
	assert.Nil(t, bound)
}
