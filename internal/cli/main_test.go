package cli_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/block/pg-sprite/internal/cli"
)

// A command line that does not parse is an operational error: exit 1 per the
// README's exit-code table, with the error and usage on stderr. Stdout stays
// empty — under --json it carries only the machine-readable product, and a
// usage error produces none.
func TestUsageErrorExitsOneAndLeavesStdoutEmpty(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{
			name: "unknown flag",
			args: []string{"migrate", "--no-such-flag"},
		},
		{
			name: "unknown flag under --json",
			args: []string{"migrate", "--json", "--no-such-flag"},
		},
		{
			name: "unknown command",
			args: []string{"no-such-command"},
		},
		{
			name: "flag combination rejected by validation under --json",
			args: []string{
				"diff", "--json", "--sql",
				"--url", "postgres://user@localhost:5432/app",
				"--desired", "main_test.go",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearFlagEnv(t)
			var stdout, stderr bytes.Buffer

			code := cli.Main(tt.args, "test", &stdout, &stderr)

			assert.Equal(t, 1, code, "a usage error exits 1, the README's operational-error code")
			assert.Empty(t, stdout.String(), "stdout is the product; a usage error has none")
			assert.NotEmpty(t, stderr.String(), "the error and usage land on stderr")
		})
	}
}
