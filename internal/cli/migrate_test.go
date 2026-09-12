package cli

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/alecthomas/kong"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/verdict"
)

// parseMigrate runs args through the real command grammar so these tests
// exercise the same flag-to-field path production does.
func parseMigrate(t *testing.T, args ...string) *MigrateCmd {
	t.Helper()
	c := New("test")
	k, err := kong.New(c, kong.Vars{"version": "test"})
	require.NoError(t, err)
	_, err = k.Parse(append([]string{
		"migrate",
		"--url", "postgres://user@localhost:5432/app",
		"--alter", "ALTER TABLE t ADD COLUMN c int",
	}, args...))
	require.NoError(t, err)
	return &c.Migrate
}

// A dry run reports the unforced plan, so pairing it with the --force
// acknowledgement is a contradiction the grammar rejects up front rather
// than silently ignoring the override.
func TestForceRejectedWithDryRun(t *testing.T) {
	c := New("test")
	k, err := kong.New(c, kong.Vars{"version": "test"})
	require.NoError(t, err)
	_, err = k.Parse([]string{
		"migrate",
		"--url", "postgres://user@localhost:5432/app",
		"--alter", "ALTER TABLE t ALTER COLUMN c TYPE text",
		"--dry-run",
		"--force", "public.t",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--force cannot be combined with --dry-run")
}

func TestRetryFlagsWireIntoRetryPolicy(t *testing.T) {
	c := parseMigrate(t,
		"--lock-attempts", "5",
		"--lock-backoff", "250ms",
		"--lock-backoff-max", "2s",
	)
	// Full-struct equality: swapping the backoff fields, inverting the
	// zero-value fallback, or dropping the passthrough must all fail here.
	assert.Equal(t, executor.RetryPolicy{
		MaxAttempts:    5,
		InitialBackoff: 250 * time.Millisecond,
		MaxBackoff:     2 * time.Second,
	}, c.retryPolicy())
}

// The library's sanctioned starting point and this front door's flag
// defaults are one policy by contract: an embedder starting from
// migrate.DefaultOptions and an operator running the CLI with no flags get
// identical budgets, size guard, and retry. Full-struct equality on each
// policy field, so a drift on either side fails here.
func TestMigrateDefaultsMatchLibraryDefaults(t *testing.T) {
	c := parseMigrate(t)
	got := c.options(nil)
	want := migrate.DefaultOptions()
	assert.Equal(t, want.MaxTableSizeBytes, got.MaxTableSizeBytes)
	assert.Equal(t, want.Budget, got.Budget)
	assert.Equal(t, want.Retry, got.Retry)
}

// The exit-code ladder is a constant↔behavior contract, not only a
// constant↔docs one: each verdict outcome must reach the entry point as the
// sentinel that maps to its documented code. The two committing outcomes
// are the pair that must never be confused — an accepted-blocking commit
// returning nil would exit 0 and claim online safety it does not have. A
// failed verdict returns nil from emit by design; its caller returns the run
// error, which exits 1.
func TestEmitReturnsTheSentinelForEachOutcome(t *testing.T) {
	for _, tc := range []struct {
		outcome verdict.Outcome
		want    error
	}{
		{outcome: verdict.OutcomeExecuted, want: nil},
		{outcome: verdict.OutcomeExecutedWithoutOnlineSafety, want: verdict.ErrAcceptedBlocking},
		{outcome: verdict.OutcomeRefused, want: verdict.ErrRefused},
		{outcome: verdict.OutcomeFailed, want: nil},
	} {
		t.Run(string(tc.outcome), func(t *testing.T) {
			c := parseMigrate(t, "--json")
			var out bytes.Buffer
			got := c.emit(&out, verdict.Verdict{Outcome: tc.outcome, Statement: "ALTER TABLE t ADD COLUMN c int"})
			if tc.want == nil {
				assert.NoError(t, got)
			} else {
				assert.ErrorIs(t, got, tc.want)
			}
			assert.Contains(t, out.String(), fmt.Sprintf(`"outcome": %q`, tc.outcome), "the verdict is printed before the sentinel returns")
		})
	}
}

func TestRetryPolicyDefaults(t *testing.T) {
	t.Run("kong defaults match the executor defaults", func(t *testing.T) {
		c := parseMigrate(t)
		assert.Equal(t, executor.DefaultRetryPolicy(), c.retryPolicy())
	})

	t.Run("zero-valued command falls back to the executor defaults", func(t *testing.T) {
		var c MigrateCmd
		assert.Equal(t, executor.DefaultRetryPolicy(), c.retryPolicy())
	})
}
