package change

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/decode"
)

// A position is the LSN's X/Y text, and reads back to the same LSN.
func TestPositionRoundTripsAnLSN(t *testing.T) {
	lsn := decode.LSN(0x16B3748)
	pos := formatPosition(lsn)
	assert.Equal(t, "0/16B3748", pos)

	back, err := parsePosition(pos)
	require.NoError(t, err)
	assert.Equal(t, lsn, back)
}

// The zero LSN is the empty position, and the empty position names nothing
// to resume from.
func TestEmptyPositionIsNotResumable(t *testing.T) {
	assert.Empty(t, formatPosition(0))

	_, err := parsePosition("")
	require.ErrorIs(t, err, ErrPositionNotFound)
}

// A position that is not an LSN is an error.
func TestMalformedPositionIsRefused(t *testing.T) {
	_, err := parsePosition("mysql-bin.000001:4")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrPositionNotFound)
}
