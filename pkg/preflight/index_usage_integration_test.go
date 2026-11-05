package preflight_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/preflight"
)

// indexOID resolves a fixture index to the OID ReadIndexUsage is keyed on.
func (f usageFixture) indexOID(t *testing.T, index string) uint32 {
	t.Helper()
	var oid uint32
	require.NoError(t, f.pool.QueryRow(t.Context(),
		"SELECT to_regclass($1)::oid", f.schema+"."+index).Scan(&oid))
	return oid
}

// ReadIndexUsage reports each index's own scan count, not the table's: of
// two indexes on one table, the one the test scans three times reads 3 and
// the unique one nothing scans reads 0. It reads the uniqueness of each,
// and, on PostgreSQL 16 and later, when the scanned one was last used.
func TestReadIndexUsageCountsEachIndexsOwnScans(t *testing.T) {
	f := newUsageFixture(t, testutil.StartPostgres(t))
	f.exec(t, fmt.Sprintf(`
		CREATE TABLE %[1]s.orders (
			id int PRIMARY KEY,
			sku text NOT NULL,
			created_at timestamptz NOT NULL
		);
		INSERT INTO %[1]s.orders
		SELECT g, 'sku-' || g, '2026-01-01'::timestamptz + g * interval '1 minute'
		FROM generate_series(1, 1000) g;
		CREATE INDEX orders_created_idx ON %[1]s.orders (created_at);
		CREATE UNIQUE INDEX orders_sku_key ON %[1]s.orders (sku);
		ANALYZE %[1]s.orders`, f.schema))

	// Seq scans are disabled so each lookup scans orders_created_idx, the
	// only index on created_at.
	f.exec(t, "SET enable_seqscan = off")
	for range 3 {
		f.exec(t, fmt.Sprintf(`
			SELECT id
			FROM %s.orders
			WHERE created_at = '2026-01-01 00:10:00+00'`, f.schema))
	}
	f.exec(t, "RESET enable_seqscan")
	f.publish(t)

	scanned := f.indexOID(t, "orders_created_idx")
	var u preflight.IndexUsage
	var readErr error
	require.Eventually(t, func() bool {
		if f.major < 15 {
			if _, readErr = f.conn.Exec(t.Context(), "SELECT 1"); readErr != nil {
				return true
			}
		}
		u, readErr = preflight.ReadIndexUsage(t.Context(), f.pool, scanned)
		return readErr != nil || u.IdxScans == 3
	}, statsVisible, 100*time.Millisecond, "the server must publish 3 scans of orders_created_idx")
	require.NoError(t, readErr)

	assert.EqualValues(t, 3, u.IdxScans)
	assert.False(t, u.Unique)
	assert.True(t, u.TrackCounts)
	assert.False(t, u.ReadAt.IsZero())
	if f.major >= 16 {
		require.NotNil(t, u.LastIdxScan, "PostgreSQL 16 and later record when the index was last scanned")
		assert.False(t, u.LastIdxScan.After(u.ReadAt), "the last scan cannot be later than the read")
	} else {
		assert.Nil(t, u.LastIdxScan, "PostgreSQL 14 and 15 record no scan times")
	}

	unused, err := preflight.ReadIndexUsage(t.Context(), f.pool, f.indexOID(t, "orders_sku_key"))
	require.NoError(t, err)
	assert.EqualValues(t, 0, unused.IdxScans)
	assert.True(t, unused.Unique)
	assert.Nil(t, unused.LastIdxScan)
}
