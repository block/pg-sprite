package cli

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/alecthomas/kong"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/migrate"
)

// acknowledgementFlags parses a plain migrate command line and returns
// it with every flag declared with the SCHEMA.TABLE placeholder. That
// placeholder is the convention marking a typed acknowledgement flag: a
// flag declared with it is found without this test naming it, and one
// declared without it is not seen by this walk. Each flag's value is bound
// to the returned command.
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

// Every acknowledgement flag on migrate reaches its own acknowledgement in
// the engine's irreversible-operations registry, which desired-state
// execution rejects with that acknowledgement's sentinel, and the CLI
// itself rejects the flag with --desired before reading the file. A new
// acknowledgement flag fails here until it is given its sentinel below and
// the engine has a registry entry for it; a flag wired to no engine field,
// or to another flag's field, fails because RunDesired does not return the
// flag's own sentinel. The registry is unexported, so a registry
// acknowledgement with no CLI flag is not detected here.
func TestEveryAcknowledgementFlagMapsToARegistryEntry(t *testing.T) {
	sentinels := map[string]error{
		"force":             migrate.ErrForceNotSupported,
		"accept-blocking":   migrate.ErrAcceptBlockingNotSupported,
		"allow-destructive": migrate.ErrAllowDestructiveNotSupported,
		"restart":           migrate.ErrRestartNotSupported,
		"block-writes":      migrate.ErrBlockWritesNotSupported,
	}
	_, flags := acknowledgementFlags(t)
	names := make([]string, 0, len(flags))
	for _, f := range flags {
		names = append(names, f.Name)
	}
	require.ElementsMatch(t, slices.Collect(maps.Keys(sentinels)), names,
		"the walk found the acknowledgements the engine registers")

	desired := filepath.Join(t.TempDir(), "orders.sql")
	require.NoError(t, os.WriteFile(desired, []byte("CREATE TABLE orders (id bigint PRIMARY KEY);"), 0o600))
	for i, f := range flags {
		t.Run(f.Name, func(t *testing.T) {
			c, fresh := acknowledgementFlags(t)
			fresh[i].Target.SetString("public.orders")
			_, err := migrate.RunDesired(t.Context(), nil, migrate.DesiredRequest{Schema: "public"}, c.options(nil))
			require.Error(t, err, "--%s reaches no acknowledgement desired-state execution rejects", f.Name)
			assert.ErrorIs(t, err, sentinels[f.Name], "--%s must reach its own acknowledgement", f.Name)

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
