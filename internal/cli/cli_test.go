package cli_test

import (
	"os"
	"testing"
	"time"

	"github.com/alecthomas/kong"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/cli"
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
	newKong(t, cli.New())
}

func TestMigrateFlagsWireIntoDBConfig(t *testing.T) {
	c := cli.New()
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

func TestURLIsRequiredForDatabaseCommands(t *testing.T) {
	c := cli.New()
	k := newKong(t, c)
	_, err := k.Parse([]string{"migrate", "--alter", "ALTER TABLE t ADD COLUMN c int"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--url")
}

func TestFmtIsOffline(t *testing.T) {
	c := cli.New()
	k := newKong(t, c)
	_, err := k.Parse([]string{"fmt"})
	require.NoError(t, err, "fmt must not require database flags")
}
