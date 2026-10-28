package supabase_test

import (
	"testing"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// copySwapOptions is the engine policy for a statement the router runs
// through the copy-and-swap backend, declared quiesced and with no free disk
// reported: the route's preflight then refuses before its lock session, copy
// workers, or replication stream open, which is the fail-closed shape these
// probes rely on to leave the fixture untouched. The only session opened
// from the connection is the read-only one that proves it reaches the
// pool's database. BlockWrites, acknowledged for st's table, keeps the probe
// independent of the fixture role's replication access.
func copySwapOptions(st statement.Statement) migrate.Options {
	opts := migrate.DefaultOptions()
	opts.CopySwap = migrate.CopySwapOptions{
		Connection:  dbconn.Config{URL: directURL},
		BlockWrites: st.Schema() + "." + st.Table(),
	}
	return opts
}

// refuseCarriedCopySwap runs a statement the engine carries through the
// copy-and-swap backend and proves the route refuses fail-closed at its
// preflight: the verdict carries the preflight's own cause — want names the
// first fact about the table or environment the route refuses on — and the
// table is identical before and after.
func refuseCarriedCopySwap(t *testing.T, pool *pgxpool.Pool, table, sql string, want preflight.CopySwapRefusalCause) {
	t.Helper()
	before := snapshot(t, pool, table)
	st, err := statement.ParseOne(sql)
	require.NoError(t, err)
	v, err := migrate.Run(t.Context(), pool, st, copySwapOptions(st))
	require.NoError(t, err)
	require.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonCopyAndSwapRefused, v.Reason)
	assert.Equal(t, verdict.Cause(want), v.Cause)
	assert.Empty(t, v.ExecutedSQL)
	assert.Equal(t, before, snapshot(t, pool, table), "a preflight refusal must have no side effects")
}
