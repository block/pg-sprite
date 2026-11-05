package cli_test

import (
	"os"
	"testing"
	"time"

	"github.com/alecthomas/kong"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/cli"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/migrate"
)

func newKong(t *testing.T, c *cli.CLI) *kong.Kong {
	t.Helper()
	clearFlagEnv(t)
	k, err := kong.New(c, kong.Vars{"version": "test"})
	require.NoError(t, err, "the command grammar must construct — a bad tag fails here, not in production")
	return k
}

// clearFlagEnv isolates parse tests from the caller's shell: flags bound to
// PGSPRITE_* environment variables would otherwise resolve from whatever the
// developer last exported, making required-flag and default-value assertions
// depend on the environment.
func clearFlagEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"PGSPRITE_URL", "PGSPRITE_CA_CERT"} {
		t.Setenv(key, "")
		require.NoError(t, os.Unsetenv(key))
	}
}

func TestGrammarIsValid(t *testing.T) {
	newKong(t, cli.New("test"))
}

func TestMigrateFlagsWireIntoDBConfig(t *testing.T) {
	c := cli.New("test")
	k := newKong(t, c)
	_, err := k.Parse([]string{
		"migrate",
		"--url", "postgres://user@localhost:5432/app",
		"--alter", "ALTER TABLE t ADD COLUMN c int",
		"--lock-timeout", "5s",
	})
	require.NoError(t, err)

	cfg := c.Migrate.Config()
	assert.Equal(t, "postgres://user@localhost:5432/app", cfg.URL)
	assert.Equal(t, 5*time.Second, cfg.LockTimeout)
	assert.Equal(t, 30*time.Second, cfg.StatementTimeout, "statement_timeout keeps its default")
	assert.Empty(t, cfg.CACertPath)
}

// The CLI builds its own pool, so it raises the pool's default size to what
// a copy-and-swap run with the copier's four default workers needs: one
// connection per worker, one for the catch-up's flushes, one for checkpoint
// saves and progress reads. Without it a host with four CPUs or fewer gets
// pgxpool's default of four, which the route refuses.
func TestDBConfigRaisesThePoolToWhatCopyAndSwapNeeds(t *testing.T) {
	c := cli.New("test")
	k := newKong(t, c)
	_, err := k.Parse([]string{
		"migrate",
		"--url", "postgres://user@localhost:5432/app",
		"--alter", "ALTER TABLE t ALTER COLUMN id TYPE bigint",
	})
	require.NoError(t, err)

	cfg := c.Migrate.Config()
	assert.Equal(t, int32(6), cfg.MaxConnsFloor)
	assert.Equal(t, migrate.CopySwapMinConns(copier.Options{}), cfg.MaxConnsFloor)
	assert.Zero(t, cfg.MaxConns, "an explicit size would override the operator's pool_max_conns")
}

func TestURLIsRequiredForDatabaseCommands(t *testing.T) {
	c := cli.New("test")
	k := newKong(t, c)
	_, err := k.Parse([]string{"migrate", "--alter", "ALTER TABLE t ADD COLUMN c int"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--url")
}

func TestFmtIsOffline(t *testing.T) {
	c := cli.New("test")
	k := newKong(t, c)
	_, err := k.Parse([]string{"fmt"})
	require.NoError(t, err, "fmt must not require database flags")
}
