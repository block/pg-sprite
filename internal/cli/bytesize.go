package cli

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// byteSize is a flag type for human-readable sizes ("512MiB", "1GiB", plain
// bytes). All suffixes are binary multiples — PostgreSQL's own convention for
// size units.
type byteSize int64

// suffixMultiplier maps an accepted (lowercased) size suffix to its
// multiplier; ok is false for anything unrecognized.
func suffixMultiplier(suffix string) (mult int64, ok bool) {
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

// UnmarshalText implements encoding.TextUnmarshaler so kong can parse the
// flag directly.
func (b *byteSize) UnmarshalText(text []byte) error {
	s := strings.TrimSpace(strings.ToLower(string(text)))
	digits := strings.TrimRight(s, "bkmgit ")
	suffix := strings.TrimSpace(s[len(digits):])
	mult, ok := suffixMultiplier(suffix)
	if !ok {
		return fmt.Errorf("unknown size suffix %q in %q (use B, KiB, MiB, GiB, or TiB)", suffix, string(text))
	}
	n, err := strconv.ParseInt(strings.TrimSpace(digits), 10, 64)
	if err != nil {
		return fmt.Errorf("parse size %q: %w", string(text), err)
	}
	if n <= 0 {
		return fmt.Errorf("size must be positive, got %q", string(text))
	}
	if n > math.MaxInt64/mult {
		return fmt.Errorf("size %q overflows int64 bytes", string(text))
	}
	*b = byteSize(n * mult)
	return nil
}
