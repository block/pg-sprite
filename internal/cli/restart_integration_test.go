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
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/verdict"
)

// --restart from the front door: a relation under the shadow's name that
// no checkpoint row records is refused by a plain run, and discarded by a
// run that carries the acknowledgement — which then carries the change to
// its copy-and-swap verdict on a shadow of its own. --quiesced keeps the
// run to the write fence, so the test server needs no logical decoding.
func TestMigrateRestartDiscardsALeftoverAndRunsTheChange(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %s.t (
			id int PRIMARY KEY,
			v  text
		)`, schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(
		"INSERT INTO %s.t SELECT g, repeat('x', 100) FROM generate_series(1, 1000) g", schema))
	require.NoError(t, err)
	shadow := schemachange.ShadowName(schema, "t")
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.%s (id bigint PRIMARY KEY)", schema, shadow))
	require.NoError(t, err)

	alter := fmt.Sprintf("ALTER TABLE %s.t ALTER COLUMN id TYPE bigint", schema)
	plain := newMigrateCmd(url, alter)
	plain.FreeDisk = byteSize(testutil.UnlimitedDisk)
	plain.BlockWrites = schema + ".t"
	plain.JSON = true
	var refusedOut strings.Builder
	require.ErrorIs(t, plain.run(t.Context(), &refusedOut), verdict.ErrRefused, "a refusal verdict exits with the refusal code")
	var refused verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(refusedOut.String()), &refused))
	require.Equal(t, verdict.OutcomeRefused, refused.Outcome)
	require.Equal(t, verdict.Cause(schemachange.CauseShadowUnrecorded), refused.Cause, "a plain run will not guess what built the leftover")

	restart := newMigrateCmd(url, alter)
	restart.FreeDisk = byteSize(testutil.UnlimitedDisk)
	restart.BlockWrites = schema + ".t"
	restart.JSON = true
	restart.Restart = schema + ".t"
	var out strings.Builder
	require.NoError(t, restart.run(t.Context(), &out))

	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome)
	assert.Equal(t, schema+".t", v.Table)
	assert.False(t, v.Forced, "a restart acknowledgement is not an override")

	var typ string
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT data_type FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = 't' AND column_name = 'id'`, schema).Scan(&typ))
	assert.Equal(t, "bigint", typ, "the swapped-in table carries the new type")
	var count int
	require.NoError(t, pool.QueryRow(t.Context(),
		fmt.Sprintf("SELECT count(*) FROM %s.t", schema)).Scan(&count))
	assert.Equal(t, 1000, count, "every row is carried across the swap")
	var leftover bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1 AND c.relname = $2)`, schema, shadow).Scan(&leftover))
	assert.False(t, leftover, "the leftover went with the run that replaced it")
}

// An acknowledgement that names another table executes nothing: the
// command fails before any verdict and the leftover still stands.
func TestMigrateRestartAckMismatchExecutesNothing(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %s.t (
			id int PRIMARY KEY
		)`, schema))
	require.NoError(t, err)
	shadow := schemachange.ShadowName(schema, "t")
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.%s (id bigint PRIMARY KEY)", schema, shadow))
	require.NoError(t, err)

	cmd := newMigrateCmd(url, fmt.Sprintf("ALTER TABLE %s.t ALTER COLUMN id TYPE bigint", schema))
	cmd.FreeDisk = byteSize(testutil.UnlimitedDisk)
	cmd.BlockWrites = schema + ".t"
	cmd.Restart = "wrong.table"
	var out strings.Builder
	err = cmd.run(t.Context(), &out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "restart acknowledgement")
	assert.Empty(t, out.String(), "no verdict is written")

	var typ string
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT data_type FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = 't' AND column_name = 'id'`, schema).Scan(&typ))
	assert.Equal(t, "integer", typ, "nothing was executed")
	var leftover bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1 AND c.relname = $2)`, schema, shadow).Scan(&leftover))
	assert.True(t, leftover, "nothing was discarded")
}
