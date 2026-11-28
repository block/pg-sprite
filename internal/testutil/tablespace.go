package testutil

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// NewTablespace makes a directory on the server's own filesystem, creates a
// tablespace in it named after schema, and returns the tablespace's name.
// It writes to the server's filesystem, so the calling test must first call
// SkipOnSharedServer itself. Its cleanup drops schema, whose tables keep the
// tablespace from being dropped, and then the tablespace.
func NewTablespace(t *testing.T, pool *pgxpool.Pool, schema string) string {
	t.Helper()
	tablespace := schema + "_ts"
	// CREATE TABLESPACE needs an existing directory on the server, and COPY TO
	// PROGRAM is how a test runs a shell command there. COPY must copy some
	// query's output, so it copies SELECT 1, which nothing uses. The server
	// still writes that row to the program's stdin, and mkdir exits without
	// reading it, so the write can hit a closed pipe and fail the statement.
	// cat reads the row, so the write always has a reader. && is deliberate:
	// a failed mkdir must fail the statement, which ; would hide.
	_, err := pool.Exec(t.Context(), `COPY (SELECT 1) TO PROGRAM 'mkdir -p /tmp/`+tablespace+` && cat > /dev/null'`)
	require.NoError(t, err, "make the tablespace directory")
	_, err = pool.Exec(t.Context(), `CREATE TABLESPACE `+tablespace+` LOCATION '/tmp/`+tablespace+`'`)
	require.NoError(t, err, "create the tablespace")
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		_, err := pool.Exec(ctx, `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
		assert.NoError(t, err)
		_, err = pool.Exec(ctx, `DROP TABLESPACE `+tablespace)
		assert.NoError(t, err)
	})
	return tablespace
}
