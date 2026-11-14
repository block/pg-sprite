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

// migrateFlags parses a plain migrate command line and returns it with
// every flag the command accepts, hidden and inherited ones included, in
// declaration order. Each flag's value is bound to the returned command.
func migrateFlags(t *testing.T) (*MigrateCmd, []*kong.Flag) {
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
	for _, group := range kctx.Selected().AllFlags(false) {
		flags = append(flags, group...)
	}
	return &c.Migrate, flags
}

// stringFieldsSetTo names the string fields on opts and its copy-and-swap
// options that hold want.
func stringFieldsSetTo(opts migrate.Options, want string) []string {
	var set []string
	collect := func(v reflect.Value, prefix string) {
		for i := range v.NumField() {
			if f := v.Field(i); f.Kind() == reflect.String && f.String() == want {
				set = append(set, prefix+v.Type().Field(i).Name)
			}
		}
	}
	collect(reflect.ValueOf(opts), "Options.")
	collect(reflect.ValueOf(opts.CopySwap), "CopySwapOptions.")
	return set
}

// Every migrate flag is either a typed acknowledgement or declared not
// one, and every acknowledgement in the engine's irreversible-operations
// registry has a flag. Each acknowledgement flag holds a SCHEMA.TABLE and
// reaches its own acknowledgement and no other policy field: desired-state
// execution rejects it with that acknowledgement's sentinel, and the CLI
// itself rejects the flag with --desired before reading the file. A
// declared non-acknowledgement string or bool flag reaches no
// acknowledgement. A new flag fails here until it is classified; a new
// acknowledgement flag needs its sentinel and field below and a registry
// entry; a registry acknowledgement with no flag fails; and a flag wired
// to no engine field, to another flag's field, or to its own field and
// another fails.
func TestEveryAcknowledgementFlagMapsToARegistryEntry(t *testing.T) {
	sentinels := map[string]error{
		"force":             migrate.ErrForceNotSupported,
		"accept-blocking":   migrate.ErrAcceptBlockingNotSupported,
		"allow-destructive": migrate.ErrAllowDestructiveNotSupported,
		"restart":           migrate.ErrRestartNotSupported,
		"block-writes":      migrate.ErrBlockWritesNotSupported,
	}
	fields := map[string]string{
		"force":             "Options.Force",
		"accept-blocking":   "Options.AcceptBlocking",
		"allow-destructive": "Options.AllowDestructive",
		"restart":           "CopySwapOptions.Restart",
		"block-writes":      "CopySwapOptions.BlockWrites",
	}
	// Every migrate flag that is not a typed acknowledgement: none of them
	// accepts a loss for one table.
	notAcks := []string{
		// Inherited from the root command.
		"help", "version",
		// Connection and session bounds.
		"url", "ca-cert", "lock-timeout", "statement-timeout", "debug",
		// Output, and what to run.
		"color", "alter", "desired", "schema", "extension-schema", "dry-run", "json", "progress",
		// Native-route policy: the size guard, step bounds, and lock retry.
		"max-table-size", "index-build-timeout", "validate-timeout",
		"lock-attempts", "lock-backoff", "lock-backoff-max",
		// Copy-and-swap policy: opening the route, the disk preflight, and
		// keeping the retained source, which loses less, not more.
		"copy-and-swap", "free-disk", "keep-old",
	}
	require.ElementsMatch(t, migrate.Acknowledgements(), slices.Collect(maps.Keys(sentinels)),
		"every acknowledgement the engine takes needs a migrate flag, with its sentinel and field in this test")

	_, all := migrateFlags(t)
	var ackFlags []int
	var unclassified, flagNames []string
	for i, f := range all {
		flagNames = append(flagNames, f.Name)
		if _, ok := sentinels[f.Name]; ok {
			assert.Equal(t, reflect.String, f.Target.Kind(), "--%s must hold a table name", f.Name)
			assert.Equal(t, "SCHEMA.TABLE", f.PlaceHolder, "--%s must name the table it acknowledges", f.Name)
			ackFlags = append(ackFlags, i)
			continue
		}
		if !slices.Contains(notAcks, f.Name) {
			unclassified = append(unclassified, f.Name)
			continue
		}
		c, fresh := migrateFlags(t)
		switch f.Target.Kind() {
		case reflect.Bool:
			fresh[i].Target.SetBool(true)
		case reflect.String:
			fresh[i].Target.SetString("public.orders")
		default:
			continue
		}
		assert.Empty(t, c.options(nil).Acknowledgements(), "--%s is declared not an acknowledgement", f.Name)
	}
	require.Empty(t, unclassified,
		"a migrate flag is a typed acknowledgement or declared not one: give a new acknowledgement its "+
			"sentinel and field in this test and an entry in the engine's irreversible-operations registry, "+
			"or list it in notAcks")
	for _, name := range notAcks {
		assert.Contains(t, flagNames, name, "declared not an acknowledgement but migrate has no such flag")
	}
	require.Len(t, ackFlags, len(sentinels))

	desired := filepath.Join(t.TempDir(), "orders.sql")
	require.NoError(t, os.WriteFile(desired, []byte("CREATE TABLE orders (id bigint PRIMARY KEY);"), 0o600))
	for _, i := range ackFlags {
		f := all[i]
		t.Run(f.Name, func(t *testing.T) {
			c, fresh := migrateFlags(t)
			fresh[i].Target.SetString("public.orders")
			assert.Equal(t, []string{fields[f.Name]}, stringFieldsSetTo(c.options(nil), "public.orders"),
				"--%s must reach its own field and no other", f.Name)
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
