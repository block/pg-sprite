package decode

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A present column yields its value, and a NULL — a present column with a
// nil value — is told apart from an absent one by the presence Get returns
// with it, not by the value.
func TestColumnGetTellsNULLFromAbsence(t *testing.T) {
	value, present := PresentColumn("label", "x").Get()
	assert.Equal(t, "x", value)
	assert.True(t, present)

	value, present = PresentColumn("note", nil).Get()
	assert.Nil(t, value)
	assert.True(t, present, "a NULL is a present column")

	value, present = AbsentColumn("blob").Get()
	assert.Nil(t, value)
	assert.False(t, present, "the unchanged-TOAST marker carries no value")
}

// The constructors name the column and decide its presence; nothing else
// about a column is readable.
func TestColumnNameAndPresence(t *testing.T) {
	c := PresentColumn("label", "x")
	assert.Equal(t, "label", c.Name())
	assert.True(t, c.Present())

	c = AbsentColumn("blob")
	assert.Equal(t, "blob", c.Name())
	assert.False(t, c.Present())
}
