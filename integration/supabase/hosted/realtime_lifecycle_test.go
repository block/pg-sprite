package hosted_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// One fixture and two sockets cover the entire steady-state schema change lifecycle.
// Initialization is observable and has a separate deadline from event delivery.
func TestHostedRealtimeContinuity(t *testing.T) {
	f := newFixture(t)
	f.seed(t)
	first, second := f.subscribe(t, 0), f.subscribe(t, 1)
	if !t.Run("initialize", func(t *testing.T) {
		started := time.Now()
		// Wait for an active publication reader before sending the baseline write.
		const publicationStartupDeadline = 75 * time.Second
		require.EventuallyWithT(t, func(c *assert.CollectT) {
			var ready bool
			err := f.pool.QueryRow(t.Context(), `SELECT EXISTS (
    SELECT 1 FROM pg_replication_slots WHERE plugin='wal2json' AND active
   )`).Scan(&ready)
			require.NoError(c, err)
			assert.True(c, ready, "waiting for hosted publication reader")
		}, publicationStartupDeadline, 200*time.Millisecond)
		for _, u := range f.users {
			var count int
			require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT count(*) FROM realtime.subscription
    WHERE entity=$1::regclass AND claims->>'sub'=$2 AND claims->>'role'='authenticated'`, f.table, u.ID).Scan(&count))
			require.Equal(t, 1, count)
		}
		f.exec(t, "UPDATE "+f.table+" SET body='baseline ready'")
		first.exactRow(t, "UPDATE", 1)
		second.exactRow(t, "UPDATE", 2)
		t.Logf("reader and baseline delivery ready after %s", time.Since(started))
	}) {
		return
	}
	update := func(t *testing.T) {
		t.Helper()
		f.exec(t, "UPDATE "+f.table+" SET body=$1", t.Name())
		first.exactRow(t, "UPDATE", 1)
		second.exactRow(t, "UPDATE", 2)
		f.assertAccess(t)
	}
	if !t.Run("no_DDL_control", update) {
		return
	}
	if !t.Run("refuse_volatile_UUID", func(t *testing.T) { realtimeRefuseUUID(t, f, first, second) }) {
		return
	}
	if !t.Run("refuse_volatile_timestamp", func(t *testing.T) { realtimeRefuseTimestamp(t, f, first, second) }) {
		return
	}
	var oid uint32
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT $1::regclass::oid", f.table).Scan(&oid))
	if !t.Run("add_column", func(t *testing.T) {
		f.apply(t, "ALTER TABLE "+f.table+" ADD COLUMN title text")
		update(t)
	}) {
		return
	}
	if !t.Run("add_index", func(t *testing.T) {
		result := f.apply(t, "CREATE INDEX "+pgx.Identifier{f.name + "_body"}.Sanitize()+" ON "+f.table+" (body)")
		require.Len(t, result.ExecutedSQL, 1)
		assert.Regexp(t, `^CREATE INDEX CONCURRENTLY `, result.ExecutedSQL[0])
		update(t)
	}) {
		return
	}
	var current uint32
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT $1::regclass::oid", f.table).Scan(&current))
	assert.Equal(t, oid, current)
}

// Unlike a search for one matching row, reject an unexpected row or operation.
func (s *stream) exactRow(t *testing.T, operation string, id int) {
	t.Helper()
	s.await(t, func(e event) bool {
		if e.Event != "postgres_changes" {
			return false
		}
		require.Equal(t, operation, e.Payload.Data.Type)
		require.Equal(t, id, e.Payload.Data.Record.ID)
		return true
	})
}
