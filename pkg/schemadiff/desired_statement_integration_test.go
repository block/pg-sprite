package schemadiff_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// A desired file whose statement the server rejects on the scratch schema
// is a mistake in the file: an index on a column the file no longer
// declares, a default that calls a function that does not exist, a
// collation the server does not have, or a non-immutable function in an
// index predicate. The build returns the server's message and the statement
// that drew it, as the planner ran it, so the file can be corrected without reading a server log.
func TestIntrospectDesiredNamesTheStatementTheServerRejects(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)

	for name, tc := range map[string]struct {
		file, code, statement string
	}{
		"an index on a column the file does not declare": {
			file:      "CREATE TABLE events (id bigint PRIMARY KEY);\nCREATE INDEX events_legacy_idx ON events (legacy);",
			code:      "42703",
			statement: "CREATE INDEX events_legacy_idx ON events USING btree (legacy)",
		},
		"a default that calls a missing function": {
			file:      "CREATE TABLE events (id bigint PRIMARY KEY, token text DEFAULT gen_token());",
			code:      "42883",
			statement: "CREATE TABLE events (id bigint PRIMARY KEY, token text DEFAULT gen_token())",
		},
		"a collation the server does not have": {
			file:      `CREATE TABLE events (id bigint PRIMARY KEY, note text COLLATE "en_XX");`,
			code:      "42704",
			statement: `CREATE TABLE events (id bigint PRIMARY KEY, note text COLLATE "en_XX")`,
		},
		"a non-immutable function in an index predicate": {
			file:      "CREATE TABLE events (id bigint PRIMARY KEY, at timestamptz);\nCREATE INDEX events_recent_idx ON events (at) WHERE at > now();",
			code:      "42P17",
			statement: "CREATE INDEX events_recent_idx ON events USING btree (at) WHERE at > now()",
		},
	} {
		t.Run(name, func(t *testing.T) {
			ds, err := statement.ParseDesired(tc.file)
			require.NoError(t, err)
			_, err = schemadiff.IntrospectDesired(t.Context(), pool, schema, ds)
			require.ErrorIs(t, err, schemadiff.ErrDesiredStatementRejected)
			var rejected *schemadiff.DesiredStatementError
			require.True(t, errors.As(err, &rejected), "%v", err)
			assert.Equal(t, tc.code, rejected.Err.Code)
			var server *pgconn.PgError
			require.True(t, errors.As(err, &server))
			assert.Equal(t, tc.code, server.Code)
			assert.Contains(t, err.Error(), "in: "+tc.statement)
		})
	}
}
