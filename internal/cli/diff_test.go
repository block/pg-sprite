package cli

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/statement"
)

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

func TestFmtRefusesInvalidSQL(t *testing.T) {
	cmd := &FmtCmd{}
	var out strings.Builder
	err := cmd.runFmt(strings.NewReader("CREATE TABEL t (id int)"), &out)
	require.Error(t, err)
	assert.Empty(t, out.String())
}
