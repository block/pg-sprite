package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/block/pg-sprite/pkg/verdict"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	domainFile     = "CREATE DOMAIN email AS text;\n"
	enumTableFile  = "CREATE TYPE mood AS ENUM ('happy', 'sad');\nCREATE TABLE t (id int);\n"
	partitionsFile = "CREATE TABLE log_p1 PARTITION OF log FOR VALUES FROM (1) TO (2);\n" +
		"CREATE TABLE log_p2 PARTITION OF log FOR VALUES FROM (2) TO (3);\n"
)

func writeDesiredFile(t *testing.T, sql string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "schema.sql")
	require.NoError(t, os.WriteFile(file, []byte(sql), 0600))
	return file
}

// A domain file, a table file with an enum type in it, and a file of
// partitions are refused before anything is
// planned: diff prints the refusal verdict in each output format and exits
// with the refusal code, without connecting to the database.
func TestDiffRefusesADesiredFileBeforePlanning(t *testing.T) {
	tests := []struct {
		name  string
		sql   string
		stmt  string
		class verdict.Class
	}{
		{"domain file", domainFile, "CREATE DOMAIN email AS text", verdict.ClassNoOnlineSafetyProblem},
		{"table file with an enum type", enumTableFile, "CREATE TYPE mood AS ENUM ('happy', 'sad')", verdict.ClassNoOnlineSafetyProblem},
		{"partitions file", partitionsFile, "CREATE TABLE log_p2 PARTITION OF log FOR VALUES FROM (2) TO (3)", verdict.ClassCapabilityBoundary},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := writeDesiredFile(t, tt.sql)

			var jsonOut strings.Builder
			err := (&DiffCmd{Desired: file, JSON: true}).run(t.Context(), &jsonOut)
			require.ErrorIs(t, err, verdict.ErrRefused)
			var v verdict.Verdict
			require.NoError(t, json.Unmarshal([]byte(jsonOut.String()), &v))
			assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
			assert.Equal(t, tt.class, v.Class)
			assert.Equal(t, tt.stmt, v.Statement)

			var text strings.Builder
			err = (&DiffCmd{Desired: file, OutputFlags: OutputFlags{Color: "never"}}).run(t.Context(), &text)
			require.ErrorIs(t, err, verdict.ErrRefused)
			assert.Contains(t, text.String(), "refused (unsupported-statement)")
			assert.Contains(t, text.String(), "class:     "+string(tt.class))
			assert.Contains(t, text.String(), "statement: "+tt.stmt)

			var sqlOut strings.Builder
			err = (&DiffCmd{Desired: file, SQL: true}).run(t.Context(), &sqlOut)
			require.ErrorIs(t, err, verdict.ErrRefused)
			assert.Equal(t, "-- refused: "+v.Detail+"\n", sqlOut.String())
		})
	}
}

// migrate --desired refuses the same files the same way, before it plans,
// on a run and on a dry run.
func TestMigrateDesiredRefusesADesiredFileBeforePlanning(t *testing.T) {
	file := writeDesiredFile(t, domainFile)
	for _, dryRun := range []bool{false, true} {
		var out strings.Builder
		cmd := &MigrateCmd{Desired: file, DryRun: dryRun, JSON: true}
		err := cmd.runDesired(t.Context(), &out)
		require.ErrorIs(t, err, verdict.ErrRefused, "dry run %t", dryRun)
		var v verdict.Verdict
		require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
		assert.Equal(t, verdict.ClassNoOnlineSafetyProblem, v.Class)
		assert.Equal(t, verdict.OwnerDirectOperator, v.Owner)
		assert.Equal(t, "CREATE DOMAIN email AS text", v.Statement)
	}
}

// A desired file that refuses several statements lists every one with where
// it starts, in each output format, so its author fixes them in one edit. The
// verdict itself still describes the one refusal that decides it.
func TestDiffListsEveryStatementADesiredFileRefuses(t *testing.T) {
	file := writeDesiredFile(t, `CREATE TABLE orders (
  id bigint PRIMARY KEY
);

COMMENT ON TABLE orders IS 'Orders placed by accounts';

CREATE FUNCTION touch() RETURNS trigger AS $$ BEGIN RETURN NEW; END; $$ LANGUAGE plpgsql;
`)
	wantLines := []string{
		"line 5, column 1 (statement 2): statement kind not allowed in a desired schema: COMMENT ON TABLE orders IS 'Orders placed by accounts'",
		"line 7, column 1 (statement 3): statement kind not allowed in a desired schema: CREATE FUNCTION touch()",
	}

	var jsonOut strings.Builder
	err := (&DiffCmd{Desired: file, JSON: true}).run(t.Context(), &jsonOut)
	require.ErrorIs(t, err, verdict.ErrRefused)
	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(jsonOut.String()), &v))
	assert.Equal(t, "COMMENT ON TABLE orders IS 'Orders placed by accounts'", v.Statement)
	require.Len(t, v.RefusedStatements, 2)
	assert.Equal(t, verdict.RefusedStatement{Statement: 2, Line: 5, Column: 1,
		Detail: "statement kind not allowed in a desired schema: COMMENT ON TABLE orders IS 'Orders placed by accounts'"}, v.RefusedStatements[0])
	assert.Equal(t, 7, v.RefusedStatements[1].Line)

	var text strings.Builder
	err = (&DiffCmd{Desired: file, OutputFlags: OutputFlags{Color: "never"}}).run(t.Context(), &text)
	require.ErrorIs(t, err, verdict.ErrRefused)
	assert.Contains(t, text.String(), "\n  refused:   "+wantLines[0])
	assert.Contains(t, text.String(), "\n             "+wantLines[1])

	var sqlOut strings.Builder
	err = (&DiffCmd{Desired: file, SQL: true}).run(t.Context(), &sqlOut)
	require.ErrorIs(t, err, verdict.ErrRefused)
	assert.Contains(t, sqlOut.String(), "\n-- refused at "+wantLines[0]+"\n")
	assert.Contains(t, sqlOut.String(), "\n-- refused at "+wantLines[1])
}
