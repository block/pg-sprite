package cli

import (
	"testing"

	"github.com/alecthomas/kong"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --allow-destructive acknowledges data loss, a risk none of the other
// acknowledgements covers, so the grammar combines it with --force,
// --dry-run, --restart, and --accept-blocking (an accepted DROP INDEX
// needs both), and refuses it beside --desired, which refuses destructive
// plans whole.
func TestAllowDestructiveFlagGrammar(t *testing.T) {
	parse := func(args ...string) error {
		c := New("test")
		k, err := kong.New(c, kong.Vars{"version": "test"})
		require.NoError(t, err)
		_, err = k.Parse(append([]string{
			"migrate",
			"--url", "postgres://user@localhost:5432/app",
			"--alter", "ALTER TABLE public.orders DROP COLUMN note",
			"--allow-destructive", "public.orders",
		}, args...))
		return err
	}

	t.Run("stands alone", func(t *testing.T) {
		require.NoError(t, parse())
	})

	t.Run("combines with --force", func(t *testing.T) {
		require.NoError(t, parse("--force", "public.orders"))
	})

	t.Run("combines with --dry-run", func(t *testing.T) {
		require.NoError(t, parse("--dry-run"))
	})

	t.Run("combines with --restart", func(t *testing.T) {
		require.NoError(t, parse("--restart", "public.orders"))
	})

	t.Run("combines with --accept-blocking", func(t *testing.T) {
		require.NoError(t, parse("--accept-blocking", "public.orders", "--statement-timeout", "10m"))
	})

	t.Run("rejects --desired", func(t *testing.T) {
		c := &MigrateCmd{Desired: "orders.sql", AllowDestructive: "public.orders"}
		err := c.validateInput()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--allow-destructive cannot be combined with --desired")
	})

	t.Run("wires the acknowledgement into the engine policy", func(t *testing.T) {
		c := parseMigrate(t, "--allow-destructive", "public.orders")
		assert.Equal(t, "public.orders", c.options(nil).AllowDestructive)
	})
}
