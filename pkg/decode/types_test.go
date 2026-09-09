package decode

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestChangeKindString(t *testing.T) {
	assert.Equal(t, "insert", Insert.String())
	assert.Equal(t, "update", Update.String())
	assert.Equal(t, "delete", Delete.String())
	assert.Equal(t, "ChangeKind(99)", ChangeKind(99).String())
}
func TestLSNString(t *testing.T) { assert.Equal(t, "16/B374D848", LSN(0x00000016b374d848).String()) }
func TestPresentColumns(t *testing.T) {
	e := ChangeEvent{Columns: []Column{{Name: "blob", Present: false}, {Name: "label", Value: "x", Present: true}}}
	assert.Equal(t, []Column{{Name: "label", Value: "x", Present: true}}, e.PresentColumns())
}
