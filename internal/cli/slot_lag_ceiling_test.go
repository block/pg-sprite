package cli

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
)

// --slot-lag-ceiling is how an operator raises the WAL a live
// copy-and-swap's slot may hold: it reaches the engine policy as the exact
// byte count, a plain integer of bytes or a whole number of a binary unit.
// Left off, the policy carries zero, which the engine reads as its one GiB
// default.
func TestSlotLagCeilingFlagReachesTheCopySwapPolicy(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int64
	}{
		{value: "2GiB", want: 2 << 30},
		{value: "3gb", want: 3 << 30},
		{value: "512MiB", want: 512 << 20},
		{value: "1073741825", want: 1<<30 + 1},
		{value: "1B", want: 1},
	} {
		t.Run(tc.value, func(t *testing.T) {
			got := parseMigrate(t, "--copy-and-swap", "--slot-lag-ceiling", tc.value).options(nil).CopySwap
			assert.Equal(t, tc.want, got.SlotLagCeilingBytes)
		})
	}

	t.Run("left off, the engine default stands", func(t *testing.T) {
		got := parseMigrate(t, "--copy-and-swap").options(nil).CopySwap
		assert.Zero(t, got.SlotLagCeilingBytes, "zero is the engine's own one GiB default")
	})
}

// A ceiling of zero or below, a fraction, an unknown unit, or a value that
// overflows the byte count is a usage error: exit 1 with the flag named on
// stderr and nothing on stdout, before anything dials the server. Zero is
// refused rather than read as the default so a typed value always means
// the ceiling it spells.
func TestSlotLagCeilingFlagRefusesABadSize(t *testing.T) {
	for _, value := range []string{"0", "-1", "0GiB", "1.5GiB", "1PiB", "lots", "9999999TiB"} {
		t.Run(value, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Main([]string{
				"migrate", "--json", "--copy-and-swap",
				"--url", "postgres://user@127.0.0.1:1/app",
				"--alter", "ALTER TABLE t ALTER COLUMN c TYPE bigint",
				"--slot-lag-ceiling=" + value,
			}, "test", &stdout, &stderr)
			assert.Equal(t, exitFailed, code)
			assert.Empty(t, stdout.String())
			assert.Contains(t, stderr.String(), "--slot-lag-ceiling")
		})
	}
}
