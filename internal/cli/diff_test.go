package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/statement"
)

// --json and --sql each replace the default diagnostic report with a
// different whole-output format; combining them names no single output, so
// the parse is rejected rather than one flag silently winning.
func TestDiffRejectsSQLWithJSON(t *testing.T) {
	desired := filepath.Join(t.TempDir(), "schema.sql")
	require.NoError(t, os.WriteFile(desired, []byte("CREATE TABLE t (id bigint PRIMARY KEY)"), 0o600))
	c := New()
	k, err := kong.New(c, kong.Vars{"version": "test"})
	require.NoError(t, err)
	_, err = k.Parse([]string{
		"diff",
		"--url", "postgres://user@localhost:5432/app",
		"--desired", desired,
		"--sql",
		"--json",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--sql cannot be combined with --json")
}

func TestFmtCanonicalizesFromStdin(t *testing.T) {
	cmd := &FmtCmd{}
	in := strings.NewReader(`create table events (
  id bigint primary key,
  name varchar(50) not null
);
create index events_name_idx on events (name);`)
	var out strings.Builder
	require.NoError(t, cmd.runFmt(in, &out))
	assert.Equal(t,
		"CREATE TABLE events (id bigint PRIMARY KEY, name varchar(50) NOT NULL);\n"+
			"CREATE INDEX events_name_idx ON events USING btree (name);\n",
		out.String())
}

func TestFmtRefusesDisallowedStatements(t *testing.T) {
	cmd := &FmtCmd{}
	var out strings.Builder
	err := cmd.runFmt(strings.NewReader("CREATE TABLE t (id int); DELETE FROM t"), &out)
	require.ErrorIs(t, err, statement.ErrDisallowedStatement)
	assert.Empty(t, out.String(), "nothing is written when the input is refused")
}

func TestFmtRefusesCommentedInput(t *testing.T) {
	cmd := &FmtCmd{}
	var out strings.Builder
	err := cmd.runFmt(strings.NewReader(`-- events: one row per business event
CREATE TABLE events (
  id bigint PRIMARY KEY,
  name varchar(50) NOT NULL -- display name
);
-- covering index for the dashboard query
CREATE INDEX events_name_idx ON events (name);`), &out)
	require.ErrorIs(t, err, statement.ErrCommentLoss)
	assert.Empty(t, out.String(), "a formatter must never emit output that lost content")
}

func TestFmtRefusesInvalidSQL(t *testing.T) {
	cmd := &FmtCmd{}
	var out strings.Builder
	err := cmd.runFmt(strings.NewReader("CREATE TABEL t (id int)"), &out)
	require.Error(t, err)
	assert.Empty(t, out.String())
}
