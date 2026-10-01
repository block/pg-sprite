package decode

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChangeKindString(t *testing.T) {
	assert.Equal(t, "insert", Insert.String())
	assert.Equal(t, "update", Update.String())
	assert.Equal(t, "delete", Delete.String())
	assert.Equal(t, "ChangeKind(99)", ChangeKind(99).String())
}
func TestLSNString(t *testing.T) { assert.Equal(t, "16/B374D848", LSN(0x00000016b374d848).String()) }

// ParseLSN inverts String: the pg_lsn text form round-trips, including the
// zero LSN and a low half with leading zeros that String would not print.
func TestParseLSN(t *testing.T) {
	cases := map[string]LSN{
		"16/B374D848":       0x00000016b374d848,
		"0/0":               0,
		"0/00000001":        1,
		"FFFFFFFF/FFFFFFFF": 0xffffffffffffffff,
	}
	for text, want := range cases {
		got, err := ParseLSN(text)
		require.NoError(t, err, text)
		assert.Equal(t, want, got, text)
	}
	for _, bad := range []string{"", "16", "16/", "/B374D848", "1/FFFFFFFFF", "0x16/1", "16/B374D848/0"} {
		_, err := ParseLSN(bad)
		assert.Error(t, err, bad)
	}
}
func TestPresentColumns(t *testing.T) {
	e := ChangeEvent{Columns: []Column{{Name: "blob", Present: false}, {Name: "label", Value: "x", Present: true}}}
	assert.Equal(t, []Column{{Name: "label", Value: "x", Present: true}}, e.PresentColumns())
}
