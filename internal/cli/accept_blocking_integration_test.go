package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/verdict"
)

// --accept-blocking turns the gate refusal of a plain DROP INDEX into a
// bounded blocking execution: the command dials, runs the statement as
// submitted, prints the without-online-safety verdict, and returns the
// sentinel the entry point maps to its own exit code — never the refusal's
// and never the plain success's.
func TestMigrateAcceptBlockingRunsDropIndex(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.users (
			id int PRIMARY KEY,
			email text
		);
		CREATE INDEX users_email_idx ON %[1]s.users (email)`, schema))
	require.NoError(t, err)

	cmd := newMigrateCmd(url, fmt.Sprintf("DROP INDEX %s.users_email_idx", schema))
	cmd.AcceptBlocking = schema + ".users"
	cmd.JSON = true
	var out strings.Builder
	err = cmd.run(t.Context(), &out)
	require.ErrorIs(t, err, verdict.ErrAcceptedBlocking)
	require.NotErrorIs(t, err, verdict.ErrRefused)

	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeExecutedWithoutOnlineSafety, v.Outcome)
	assert.True(t, v.BlockingPassthrough)
	assert.Equal(t, schema+".users", v.Table)
	assert.Equal(t, verdict.ReasonIndexStatement, v.Reason)
	assert.Equal(t, "3s", v.LockTimeout)
	assert.Equal(t, "30s", v.StatementTimeout)

	var exists bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname = $1 AND indexname = 'users_email_idx')`,
		schema).Scan(&exists))
	assert.False(t, exists, "the accepted DROP INDEX must have committed")
}

// A false acknowledgement is a usage error, not a refusal and not a
// verdict: nothing prints and nothing executes.
func TestMigrateAcceptBlockingMismatchExecutesNothing(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.users (id int PRIMARY KEY);
		CREATE INDEX users_id_idx ON %[1]s.users (id)`, schema))
	require.NoError(t, err)

	cmd := newMigrateCmd(url, fmt.Sprintf("DROP INDEX %s.users_id_idx", schema))
	cmd.AcceptBlocking = "wrong.table"
	var out strings.Builder
	err = cmd.run(t.Context(), &out)
	require.ErrorIs(t, err, migrate.ErrAcceptBlockingMismatch)
	assert.NotErrorIs(t, err, verdict.ErrRefused)
	assert.Empty(t, out.String(), "no verdict is printed")

	var exists bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname = $1 AND indexname = 'users_id_idx')`,
		schema).Scan(&exists))
	assert.True(t, exists, "nothing must have executed")
}

// The acknowledgement applies only to the eligible refusals. A gate
// refusal outside the set — an already-concurrent form, an unsupported
// kind — still refuses before dialing, flag or no flag.
func TestMigrateAcceptBlockingLeavesIneligibleRefusalsAlone(t *testing.T) {
	for _, alter := range []string{
		"DROP INDEX CONCURRENTLY public.users_email_idx",
		"REINDEX SCHEMA public",
		"ALTER INDEX public.users_email_idx SET (fillfactor = 90)",
	} {
		t.Run(alter, func(t *testing.T) {
			// An unroutable URL proves the refusal happens before connecting.
			cmd := newMigrateCmd("postgres://nobody@localhost:1/nope", alter)
			cmd.AcceptBlocking = "public.users"
			cmd.JSON = true
			var out strings.Builder
			err := cmd.run(t.Context(), &out)
			require.ErrorIs(t, err, verdict.ErrRefused)

			var v verdict.Verdict
			require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
			assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
			assert.False(t, v.BlockingPassthrough)
		})
	}
}
