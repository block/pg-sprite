package checksum_test

import (
	"context"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
)

// Losing the table lock while a repair is in flight cancels the repair's
// transaction and Check reports the loss as an invariant violation (LK-1),
// not the cancelled statement; the half-done repair rolls back with its
// transaction, so the shadow is as the pass found it. The loss is injected
// as the repair's first statement — the shadow DELETE — starts, after the
// comparison pass has already run clean through the lock.
func TestCheckAbortsWhenTheLockIsLostMidRepair(t *testing.T) {
	f := newVerifierFixture(t)
	f.createOrders(t)
	target := f.prove(t, "orders")
	buildLock := f.lock(t, "orders")
	shadow := f.build(t, buildLock, target, `ALTER TABLE %s DROP COLUMN note`)
	f.copy(t, target, shadow, buildLock)
	require.NoError(t, buildLock.Release(t.Context()))
	f.exec(t, "UPDATE "+f.shadowName(shadow)+" SET qty = 0 WHERE id = 1500")

	lock := f.lock(t, "orders", dbconn.WithTableLockKeepalive(100*time.Millisecond))
	pool := f.hookedPool(t, "DELETE FROM", func() {
		f.terminateBackend(t, lock.BackendPID())
		const lockLossDeadline = 15 * time.Second
		select {
		case <-lock.Done():
		case <-time.After(lockLossDeadline):
			t.Errorf("lock session did not report loss within %s", lockLossDeadline)
		}
	})

	_, err := f.check(t, pool, target, shadow, lock, copier.NewWatermark(math.MaxInt64), checksum.DivergenceRepair)
	require.ErrorIs(t, err, checksum.ErrInvariantViolation)
	assert.ErrorIs(t, err, lock.Err(), "the loss the session reported is the cause")
	assert.Contains(t, err.Error(), "(LK-1)")
	qty, _ := f.shadowRow(t, shadow, 1500)
	assert.Equal(t, int64(0), qty, "the cancelled repair rolled back")
}

// hookedPool is a pool whose connections run hook once, as the first
// statement starting with prefix begins. It is a test-only pool: production
// code connects through dbconn.
func (f verifierFixture) hookedPool(t *testing.T, prefix string, hook func()) *pgxpool.Pool {
	t.Helper()
	pc, err := pgxpool.ParseConfig(f.cfg.URL)
	require.NoError(t, err)
	pc.ConnConfig.Tracer = &statementHook{prefix: prefix, hook: hook}
	pool, err := pgxpool.NewWithConfig(t.Context(), pc)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// statementHook is a pgx query tracer that runs its hook once, before the
// first statement whose text starts with prefix is sent.
type statementHook struct {
	prefix string
	hook   func()
	once   sync.Once
}

func (h *statementHook) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, h.prefix) {
		h.once.Do(h.hook)
	}
	return ctx
}

func (h *statementHook) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
