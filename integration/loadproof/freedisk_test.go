package loadproof_test

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// freeDiskUnit is the multiplier of a size suffix the CLI's --free-disk
// takes, all binary: the library proof reads LOAD_PROOF_FREE_DISK in the
// same spelling the binary proof hands the flag. ok is false for any
// other suffix.
func freeDiskUnit(suffix string) (mult int64, ok bool) {
	switch suffix {
	case "", "b":
		return 1, true
	case "kb", "kib":
		return 1 << 10, true
	case "mb", "mib":
		return 1 << 20, true
	case "gb", "gib":
		return 1 << 30, true
	case "tb", "tib":
		return 1 << 40, true
	default:
		return 0, false
	}
}

// parseFreeDisk reads a size as the CLI's --free-disk does: a positive
// whole number with an optional binary unit. Copied in small from the
// CLI's own parser (internal/cli/bytesize.go), which the test cannot
// import.
func parseFreeDisk(text string) (int64, error) {
	s := strings.TrimSpace(strings.ToLower(text))
	digits := strings.TrimRight(s, "bkmgit ")
	mult, ok := freeDiskUnit(strings.TrimSpace(s[len(digits):]))
	if !ok {
		return 0, fmt.Errorf("unknown size suffix in %q (use B, KiB, MiB, GiB, or TiB)", text)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(digits), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse size %q: %w", text, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("size must be positive, got %q", text)
	}
	if n > math.MaxInt64/mult {
		return 0, fmt.Errorf("size %q overflows int64 bytes", text)
	}
	return n * mult, nil
}

// The library proof hands migrate.Run the same byte count the binary proof
// hands --free-disk for one LOAD_PROOF_FREE_DISK value.
func TestParseFreeDiskReadsTheCLIsSpelling(t *testing.T) {
	for text, want := range map[string]int64{
		"40GiB":   40 << 30,
		"10gb":    10 << 30,
		"512 MiB": 512 << 20,
		"4096":    4096,
	} {
		got, err := parseFreeDisk(text)
		require.NoError(t, err, text)
		assert.Equal(t, want, got, text)
	}
}

// A size the CLI would refuse is refused here too, rather than read as a
// figure the preflight then trusts.
func TestParseFreeDiskRefusesWhatTheCLIRefuses(t *testing.T) {
	for _, text := range []string{"", "0GiB", "-1GiB", "40 PiB", "lots"} {
		_, err := parseFreeDisk(text)
		assert.Error(t, err, text)
	}
}
