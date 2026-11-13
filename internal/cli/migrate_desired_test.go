package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/verdict"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func desiredCommand(t *testing.T, url, schema, sql string, args ...string) *MigrateCmd {
	t.Helper()
	path := filepath.Join(t.TempDir(), "documents.sql")
	require.NoError(t, os.WriteFile(path, []byte(sql), 0600))
	root := New("test")
	parser, err := kong.New(root, kong.Vars{"version": "test"})
	require.NoError(t, err)
	argv := []string{"migrate", "--url", url, "--schema", schema, "--desired", path, "--json"}
	_, err = parser.Parse(append(argv, args...))
	require.NoError(t, err)
	return &root.Migrate
}

func TestMigrateDesiredRejectsConflictingInputs(t *testing.T) {
	for _, args := range [][]string{
		{"--alter", "ALTER TABLE documents ADD COLUMN body text"},
		{"--force", "public.documents"},
		{"--accept-blocking", "public.documents", "--statement-timeout", "1s"},
	} {
		t.Run(args[0], func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "documents.sql")
			require.NoError(t, os.WriteFile(file, []byte("CREATE TABLE documents (id int PRIMARY KEY);"), 0600))
			root := New("test")
			parser, err := kong.New(root, kong.Vars{"version": "test"})
			require.NoError(t, err)
			_, err = parser.Parse(append([]string{"migrate", "--url", "postgres://localhost/test", "--desired", file}, args...))
			require.Error(t, err)
		})
	}
}

func TestMigrateDesiredRejectsInvalidDeclarationBeforeConnecting(t *testing.T) {
	cmd := desiredCommand(t, "postgres://localhost:1/test", "public", `CREATE TABLE documents (
 id int PRIMARY KEY
 );
 CREATE POLICY readers ON documents FOR SELECT USING (true);`)
	var out strings.Builder
	require.Error(t, cmd.run(t.Context(), &out))
	assert.Empty(t, out.String())
}

func TestRowSecurityVerdictPreservesUnknownCommit(t *testing.T) {
	err := &executor.RowSecurityOutcomeUnknownError{Err: context.Canceled}
	v := rowSecurityVerdict("public.documents", executor.RowSecurityReport{}, err)
	assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
	assert.Equal(t, string(executor.CodeRowSecurityOutcomeUnknown), v.Code)
	assert.Empty(t, v.ExecutedSQL)
	assert.Equal(t, err.Error(), v.Detail)
}

func TestRowSecurityVerdictPreservesCancellation(t *testing.T) {
	v := rowSecurityVerdict("public.documents", executor.RowSecurityReport{}, context.Canceled)
	assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
	assert.Equal(t, string(executor.OutcomeCode(context.Canceled)), v.Code)
	assert.Empty(t, v.ExecutedSQL)
}

func TestMigrateDesiredWriterFailure(t *testing.T) {
	cmd := MigrateCmd{JSON: true}
	want := errors.New("closed output")
	err := cmd.emit(failingDesiredWriter{want}, rowSecurityVerdict("public.documents", executor.RowSecurityReport{}, nil))
	require.ErrorIs(t, err, want)
}

type failingDesiredWriter struct{ err error }

func (w failingDesiredWriter) Write([]byte) (int, error) { return 0, w.err }

func TestMigrateRequiresOneInput(t *testing.T) {
	root := New("test")
	parser, err := kong.New(root, kong.Vars{"version": "test"})
	require.NoError(t, err)
	_, err = parser.Parse([]string{"migrate", "--url", "postgres://localhost/test"})
	require.EqualError(t, err, "migrate: provide exactly one of --alter or --desired")
}

func TestMigrateAlterRejectsSchemaFlag(t *testing.T) {
	root := New("test")
	parser, err := kong.New(root, kong.Vars{"version": "test"})
	require.NoError(t, err)
	_, err = parser.Parse([]string{"migrate", "--url", "postgres://localhost/test", "--alter", "ALTER TABLE documents ADD COLUMN body text", "--schema", "app"})
	require.EqualError(t, err, "migrate: --schema requires --desired; qualify the table in --alter instead")
}

func TestMigrateAlterRejectsExtensionSchemaFlag(t *testing.T) {
	root := New("test")
	parser, err := kong.New(root, kong.Vars{"version": "test"})
	require.NoError(t, err)
	_, err = parser.Parse([]string{"migrate", "--url", "postgres://localhost/test", "--alter", "ALTER TABLE documents ADD COLUMN email citext", "--extension-schema", "extensions"})
	require.EqualError(t, err, "migrate: --extension-schema requires --desired; qualify extension names in --alter instead")
}

// --extension-schema is refused before connecting when it names a schema
// that cannot hold an extension, whether or not the file declares row
// security; the port refuses every connection, so reaching it would fail
// differently.
func TestMigrateDesiredRefusesExtensionSchemaBeforeConnecting(t *testing.T) {
	for name, tc := range map[string]struct {
		sql, schema, want string
	}{
		"row security": {
			sql: `CREATE TABLE documents (id int PRIMARY KEY, email citext);
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;`,
			schema: "pg_toast",
			want:   `--extension-schema: invalid extension schema: "pg_toast" is a system schema`,
		},
		"system schema": {
			sql:    "CREATE TABLE documents (id int PRIMARY KEY, email citext);",
			schema: "pg_catalog",
			want:   `--extension-schema: invalid extension schema: "pg_catalog" is a system schema`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := desiredCommand(t, "postgres://localhost:1/test", "public", tc.sql, "--extension-schema", tc.schema)
			var out strings.Builder
			err := cmd.run(t.Context(), &out)
			require.ErrorContains(t, err, tc.want)
			assert.Empty(t, out.String())
		})
	}
}
