package cli

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/alecthomas/kong"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/migrate"
)

// acknowledgementFlags parses a plain migrate command line and returns
// it with every flag declared with the SCHEMA.TABLE placeholder, the marker
// every typed acknowledgement carries, so a new acknowledgement flag is
// found without this test naming it. Each flag's value is bound to the
// returned command.
func acknowledgementFlags(t *testing.T) (*MigrateCmd, []*kong.Flag) {
	t.Helper()
	c := New("test")
	k, err := kong.New(c, kong.Vars{"version": "test"})
	require.NoError(t, err)
	kctx, err := k.Parse([]string{
		"migrate",
		"--url", "postgres://user@localhost:5432/app",
		"--alter", "ALTER TABLE t ADD COLUMN c int",
	})
	require.NoError(t, err)
	var flags []*kong.Flag
	for _, group := range kctx.Selected().AllFlags(true) {
		for _, f := range group {
			if f.PlaceHolder == "SCHEMA.TABLE" {
				require.Equal(t, reflect.String, f.Target.Kind(), "--%s must hold a table name", f.Name)
				flags = append(flags, f)
			}
		}
	}
	return &c.Migrate, flags
}

// Every acknowledgement flag on migrate reaches an acknowledgement in the
// engine's irreversible-operations registry, which desired-state execution
// rejects with that acknowledgement's own sentinel, and the CLI itself
// rejects the flag with --desired before reading the file. A new
// acknowledgement flag fails here until the engine has a registry entry for
// it; a flag wired to no engine field fails because RunDesired accepts it.
func TestEveryAcknowledgementFlagMapsToARegistryEntry(t *testing.T) {
	_, flags := acknowledgementFlags(t)
	names := make([]string, 0, len(flags))
	for _, f := range flags {
		names = append(names, f.Name)
	}
	require.ElementsMatch(t, []string{
		"force", "accept-blocking", "allow-destructive", "restart", "block-writes",
	}, names, "the walk found the acknowledgements the engine registers")

	sentinels := []error{
		migrate.ErrForceNotSupported,
		migrate.ErrAcceptBlockingNotSupported,
		migrate.ErrAllowDestructiveNotSupported,
		migrate.ErrRestartNotSupported,
		migrate.ErrBlockWritesNotSupported,
	}
	desired := filepath.Join(t.TempDir(), "orders.sql")
	require.NoError(t, os.WriteFile(desired, []byte("CREATE TABLE orders (id bigint PRIMARY KEY);"), 0o600))
	seen := map[error]string{}
	for i, f := range flags {
		t.Run(f.Name, func(t *testing.T) {
			c, fresh := acknowledgementFlags(t)
			fresh[i].Target.SetString("public.orders")
			_, err := migrate.RunDesired(t.Context(), nil, migrate.DesiredRequest{Schema: "public"}, c.options(nil))
			require.Error(t, err, "--%s reaches no acknowledgement desired-state execution rejects", f.Name)
			var matched error
			for _, s := range sentinels {
				if errors.Is(err, s) {
					matched = s
				}
			}
			require.NotNil(t, matched, "--%s is rejected, but not as an acknowledgement: %v", f.Name, err)
			prev, dup := seen[matched]
			assert.False(t, dup, "--%s and --%s reach the same acknowledgement", prev, f.Name)
			seen[matched] = f.Name

			cli := New("test")
			k, err := kong.New(cli, kong.Vars{"version": "test"})
			require.NoError(t, err)
			_, err = k.Parse([]string{
				"migrate",
				"--url", "postgres://user@localhost:5432/app",
				"--desired", desired,
				"--" + f.Name, "public.orders",
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "--"+f.Name+" cannot be combined with --desired")
		})
	}
}
