package copier

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// erringHolder is a lock confirmation surface whose one query fails before
// the server answers, the way a reset connection or an expired statement
// budget fails it.
type erringHolder struct{ err error }

func (h erringHolder) QueryRow(context.Context, string, ...any) pgx.Row { return erringRow(h) }

type erringRow struct{ err error }

func (r erringRow) Scan(...any) error { return r.err }

// The copier calls a lock confirmation an invariant violation only when the
// server said the session does not hold the table (LK-1). A confirmation
// that never got the server's answer is the connection's error and is
// reported as such, so an operator reading the failure does not go looking
// for a lost lock the session still holds.
func TestConfirmLockReportsOnlyADeniedLockAsAViolation(t *testing.T) {
	f := newChunkerFixture(t)
	f.exec(t, `
		CREATE TABLE %s.orders (
			id bigint PRIMARY KEY,
			qty integer NOT NULL
		)`)
	target := f.prove(t, "orders")
	lock, err := dbconn.AcquireTableLock(t.Context(), f.cfg, f.schema, "orders")
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, lock.Release(context.WithoutCancel(t.Context()))) })
	shadow := fakeShadow{
		schema: f.schema, source: "orders", shadow: "_pgsprite_orders_new",
		sourceOID: 1, shadowOID: 2,
		columns: []string{"id", "qty"},
	}
	c, err := NewCopier(target, shadow, lock, Watermark{}, Options{})
	require.NoError(t, err)

	reset := errors.New("connection reset by peer")
	err = c.confirmLock(t.Context(), erringHolder{err: reset})
	require.ErrorIs(t, err, reset, "the connection's error is the cause")
	assert.NotErrorIs(t, err, ErrInvariantViolation, "no server answer is not a denied lock")

	require.NoError(t, lock.Release(t.Context()))
	conn, err := f.pool.Acquire(t.Context())
	require.NoError(t, err)
	t.Cleanup(conn.Release)
	err = c.confirmLock(t.Context(), conn)
	require.ErrorIs(t, err, ErrInvariantViolation, "the server saw no holder")
	assert.ErrorIs(t, err, dbconn.ErrTableLockNotHeld)
	assert.Contains(t, err.Error(), "(LK-1)")
}
