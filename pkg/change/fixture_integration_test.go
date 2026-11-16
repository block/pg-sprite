package change_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/change"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/preflight"
)

// sourceFixture is one database on a cluster that decodes WAL, holding a
// populated ledger table and the copy-and-swap target proof minted for it.
type sourceFixture struct {
	cfg    dbconn.Config
	pool   *pgxpool.Pool
	schema string
	target preflight.CopySwapTarget
}

// newSourceFixture starts a dedicated server at wal_level = logical — the
// shared harness does not decode WAL — and mints the target on a database of
// its own, so the slot name belongs to this test.
func newSourceFixture(t *testing.T) sourceFixture {
	t.Helper()
	serverURL := testutil.StartPostgresWithSettings(t, "wal_level=logical")
	cfg := dbconn.Config{URL: testutil.NewDatabase(t, serverURL)}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %s.ledger (
			id bigint PRIMARY KEY,
			note text
		)`, schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		INSERT INTO %s.ledger (id, note)
		SELECT g, 'row ' || g FROM generate_series(1, 100) AS g`, schema))
	require.NoError(t, err)

	req := preflight.Requirement{Tier: preflight.TierCopyAndSwap, LogicalDecoding: true}
	role, err := preflight.CheckPrivileges(t.Context(), pool, schema, "ledger", req)
	require.NoError(t, err)
	shape, err := preflight.CheckCopySwapShape(t.Context(), pool, schema, "ledger", role)
	require.NoError(t, err)
	target, err := preflight.CheckCopySwapEnvironment(t.Context(), pool, shape,
		preflight.CopySwapEnvironment{LogicalDecoding: true, FreeDiskBytes: testutil.UnlimitedDisk})
	require.NoError(t, err)

	f := sourceFixture{cfg: cfg, pool: pool, schema: schema, target: target}
	t.Cleanup(func() {
		assert.NoError(t, decode.DropSlot(context.WithoutCancel(t.Context()), f.cfg, f.pool, target.DecodingName()))
	})
	return f
}

// exec runs sql with the fixture's schema substituted for %s.
func (f sourceFixture) exec(t *testing.T, sql string) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), fmt.Sprintf(sql, f.schema))
	require.NoError(t, err)
}

// table is the ledger as a subscription describes it.
func (f sourceFixture) table() *change.TableInfo {
	return &change.TableInfo{SchemaName: f.schema, TableName: "ledger", Columns: []string{"id", "note"}, KeyColumns: []string{"id"}}
}

// newSource builds a source over the fixture's target that hands its
// changes to sub, closed when the test ends.
func (f sourceFixture) newSource(t *testing.T, sub *recordingSubscription, onFatal func(error)) *change.LogicalSource {
	t.Helper()
	src, err := change.NewLogicalSource(change.LogicalConfig{
		Conn:   f.cfg,
		Pool:   f.pool,
		Target: f.target,
		NewSubscription: func(currentTable, _ *change.TableInfo, _ change.MappedChunker) (change.Subscription, error) {
			sub.tables = []*change.TableInfo{currentTable}
			return sub, nil
		},
		OnFatal:  onFatal,
		ReadWait: 50 * time.Millisecond,
	})
	require.NoError(t, err)
	t.Cleanup(src.Close)
	return src
}

// started builds a source with a fresh subscription and starts it.
func (f sourceFixture) started(t *testing.T) (*change.LogicalSource, *recordingSubscription) {
	t.Helper()
	sub := newRecordingSubscription()
	src := f.newSource(t, sub, nil)
	require.NoError(t, src.AddSubscription(f.table(), nil, nil))
	require.NoError(t, src.Start(t.Context()))
	return src, sub
}

// slotConfirmed is the slot's confirmed_flush_lsn.
func (f sourceFixture) slotConfirmed(t *testing.T) decode.LSN {
	t.Helper()
	status, found, err := decode.InspectSlot(t.Context(), f.pool, f.target.DecodingName())
	require.NoError(t, err)
	require.True(t, found)
	return status.ConfirmedFlushLSN
}

// awaitSlotConfirmed waits for the slot to be confirmed to at least lsn.
func (f sourceFixture) awaitSlotConfirmed(t *testing.T, lsn decode.LSN) {
	t.Helper()
	const confirmDeadline = 10 * time.Second
	require.Eventually(t, func() bool { return f.slotConfirmed(t) >= lsn },
		confirmDeadline, 20*time.Millisecond, "slot confirmed to %s", lsn)
}

// recorded is one HasChanged call.
type recorded struct {
	key     []any
	row     []any
	deleted bool
}

// recordingSubscription keeps every change it is handed: pending until a
// flush, applied after.
type recordingSubscription struct {
	mu        sync.Mutex
	tables    []*change.TableInfo
	immutable int
	pending   []recorded
	applied   []recorded
	underLock []bool
	closed    bool
}

func newRecordingSubscription() *recordingSubscription {
	return &recordingSubscription{immutable: -1}
}

func (s *recordingSubscription) HasChanged(key, row []any, deleted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = append(s.pending, recorded{key: key, row: row, deleted: deleted})
}

func (s *recordingSubscription) Length() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}

func (s *recordingSubscription) Flush(_ context.Context, underLock bool, _ []change.TableLock) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applied = append(s.applied, s.pending...)
	s.pending = nil
	s.underLock = append(s.underLock, underLock)
	return true, nil
}

func (s *recordingSubscription) Tables() []*change.TableInfo { return s.tables }

func (s *recordingSubscription) ImmutableColumnOrdinal() int { return s.immutable }

func (s *recordingSubscription) SetWatermarkOptimization(context.Context, bool) error { return nil }

func (s *recordingSubscription) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
}

// all is every change handed over so far, applied first.
func (s *recordingSubscription) all() []recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Concat(s.applied, s.pending)
}

// flushedUnderLock is whether each flush so far ran under a lock.
func (s *recordingSubscription) flushedUnderLock() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.underLock)
}

func image(id int64, note string) recorded {
	return recorded{key: []any{id}, row: []any{fmt.Sprint(id), note}}
}

func deletion(id int64) recorded {
	return recorded{key: []any{id}, deleted: true}
}
