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

	t.Run("with --desired requires the pinned plan", func(t *testing.T) {
		c := &MigrateCmd{Desired: "orders.sql", AllowDestructive: "public.orders"}
		err := c.validateInput()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--allow-destructive with --desired requires --expect-fingerprint")
	})

	t.Run("with --desired and the pinned plan is accepted", func(t *testing.T) {
		c := &MigrateCmd{Desired: "orders.sql", AllowDestructive: "public.orders", ExpectFingerprint: "sha256:reviewed"}
		require.NoError(t, c.validateInput())
	})

	t.Run("the pinned plan stands alone with --desired", func(t *testing.T) {
		c := &MigrateCmd{Desired: "orders.sql", ExpectFingerprint: "sha256:reviewed"}
		require.NoError(t, c.validateInput())
	})

	t.Run("the pin needs --desired", func(t *testing.T) {
		c := &MigrateCmd{Alter: "ALTER TABLE public.orders DROP COLUMN note", ExpectFingerprint: "sha256:reviewed"}
		err := c.validateInput()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--expect-fingerprint requires --desired")
	})

	t.Run("the other acknowledgements stay rejected beside the pin", func(t *testing.T) {
		for flag, c := range map[string]*MigrateCmd{
			"--force":           {Desired: "orders.sql", ExpectFingerprint: "sha256:r", Force: "public.orders"},
			"--accept-blocking": {Desired: "orders.sql", ExpectFingerprint: "sha256:r", AcceptBlocking: "public.orders"},
			"--restart":         {Desired: "orders.sql", ExpectFingerprint: "sha256:r", Restart: "public.orders"},
			"--block-writes":    {Desired: "orders.sql", ExpectFingerprint: "sha256:r", BlockWrites: "public.orders"},
		} {
			err := c.validateInput()
			require.Error(t, err, flag)
			assert.Contains(t, err.Error(), flag+" cannot be combined with --desired")
		}
	})

	t.Run("wires the acknowledgement into the engine policy", func(t *testing.T) {
		c := parseMigrate(t, "--allow-destructive", "public.orders")
		assert.Equal(t, "public.orders", c.options(nil).AllowDestructive)
	})
}
