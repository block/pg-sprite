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

// Main's stderr is the CLI entry point's own rendering surface, so these
// assert its usage header and error line — not incidental prose.
func TestMainReportsUsageAndErrorOnStderr(t *testing.T) {
	clearFlagEnv(t)
	var stdout, stderr bytes.Buffer

	code := cli.Main([]string{"migrate", "--json", "--no-such-flag"}, "test", &stdout, &stderr)

	assert.Equal(t, 1, code)
	assert.Empty(t, stdout.String())
	assert.Contains(t, stderr.String(), "Usage: pg-sprite migrate")
	assert.Contains(t, stderr.String(), "pg-sprite: error: unknown flag --no-such-flag")
}

// risky.sql carries one error-severity finding, so lint returns an error
// after parsing succeeded: an operational failure, exit 1, reported on stderr.
func TestMainMapsARunErrorToExitOneWithTheErrorOnStderr(t *testing.T) {
	clearFlagEnv(t)
	var stdout, stderr bytes.Buffer

	code := cli.Main([]string{"lint", "--json", "../../demo/risky.sql"}, "test", &stdout, &stderr)

	assert.Equal(t, 1, code)
	assert.Contains(t, stderr.String(), "pg-sprite: error: lint found errors: 1")
	assert.NotContains(t, stderr.String(), "Usage:", "a run error is not a usage error")
}

// --help and --version end inside kong's parse; Main returns 0 for both
// instead of exiting the process, with their output on stdout.
func TestHelpAndVersionReturnZero(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantStdout string
	}{
		{
			name:       "help",
			args:       []string{"--help"},
			wantStdout: "Usage: pg-sprite",
		},
		{
			name:       "version",
			args:       []string{"--version"},
			wantStdout: "v9.8.7-test",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearFlagEnv(t)
			var stdout, stderr bytes.Buffer

			code := cli.Main(tt.args, "v9.8.7-test", &stdout, &stderr)

			assert.Equal(t, 0, code)
			assert.Contains(t, stdout.String(), tt.wantStdout)
			assert.Empty(t, stderr.String())
		})
	}
}
