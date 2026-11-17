package dbconn

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// SHOW renders a millisecond setting in the largest unit that divides it
// exactly, and the read-back converts each of those renderings to the
// milliseconds the pool set. These are the renderings the server returns for
// each value.
func TestShownMillisecondsReadsEveryUnitPostgreSQLShows(t *testing.T) {
	for shown, ms := range map[string]int64{
		"0":            0,
		"1ms":          1,
		"1500ms":       1500,
		"1s":           1000,
		"90s":          90_000,
		"1min":         60_000,
		"90min":        5_400_000,
		"1h":           3_600_000,
		"1d":           86_400_000,
		"2147483647ms": 2_147_483_647,
	} {
		got, err := shownMilliseconds(shown)
		if assert.NoError(t, err, shown) {
			assert.Equal(t, ms, got, shown)
		}
	}
}

// Text that is not a timeout as PostgreSQL shows one is an error, never a
// guessed bound, so the pool refuses to open rather than trusting it.
func TestShownMillisecondsRefusesTextThatIsNotAShownTimeout(t *testing.T) {
	for _, shown := range []string{
		"",
		"ms",
		"1.5s",
		"3 s",
		"500us",
		"-1ms",
		"9223372036854775807d",
	} {
		_, err := shownMilliseconds(shown)
		assert.Error(t, err, shown)
	}
}
