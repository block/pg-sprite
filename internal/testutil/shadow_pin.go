package testutil

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ShadowPin is a row inserted into a shadow table by a transaction left
// open, standing in for an applier write that is under way while the copy
// runs: the copier's insert of the chunk holding the key waits on it, so
// that chunk stays in flight until the pin is committed or released.
//
// The pin is placed at its clock's first reading, which the copier takes
// after the resume clear and the first claim, before the first chunk's
// transaction: the pin is a concurrent writer the copy meets, not a
// straggler the clear's fence waits for. A test that ends before
// the copier reads the clock has no pin: the cleanup disarms the clock
// instead of waiting on a reading that will not come.
type ShadowPin struct {
	clock *HookedClock
	// pinned is closed once tx and err are set, or once the cleanup has
	// disarmed a clock that was never read.
	pinned chan struct{}
	// tx is the pin's open transaction, set only when the insert succeeded.
	tx    pgx.Tx
	err   error
	ended sync.Once
}

// PinShadowKey arranges for key to be pinned with image qty in shadow, a
// table with columns id and qty, at the first reading of the pin's Clock.
// Callers Wait before relying on the pin and end it with Release or
// Commit; a pin still open at cleanup is rolled back, and a pin that could
// not be placed fails the test.
func PinShadowKey(t *testing.T, pool *pgxpool.Pool, shadow pgx.Identifier, key, qty int64) *ShadowPin {
	t.Helper()
	pin := newShadowPin(t.Context(), pool, shadow, key, qty)
	t.Cleanup(func() { assert.NoError(t, pin.end(context.WithoutCancel(t.Context()))) })
	return pin
}

func newShadowPin(ctx context.Context, pool *pgxpool.Pool, shadow pgx.Identifier, key, qty int64) *ShadowPin {
	pin := &ShadowPin{pinned: make(chan struct{})}
	insert := "INSERT INTO " + shadow.Sanitize() + " (id, qty) VALUES ($1, $2)"
	pin.clock = NewHookedClock(func() {
		defer close(pin.pinned)
		// The hook runs on a copier worker; failures are reported through
		// err and checked on the test goroutine.
		pin.tx, pin.err = placePin(ctx, pool, insert, key, qty)
	})
	return pin
}

// placePin begins the pin's transaction and inserts the key. A failed
// insert is rolled back here: the aborted transaction still holds its
// pooled connection, and pool.Close waits on it.
func placePin(ctx context.Context, pool *pgxpool.Pool, insert string, key, qty int64) (pgx.Tx, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin the pin: %w", err)
	}
	if _, err := tx.Exec(ctx, insert, key, qty); err != nil {
		pinErr := fmt.Errorf("pin key %d: %w", key, err)
		if rollbackErr := tx.Rollback(context.WithoutCancel(ctx)); rollbackErr != nil {
			return nil, errors.Join(pinErr, fmt.Errorf("roll back the failed pin of key %d: %w", key, rollbackErr))
		}
		return nil, pinErr
	}
	return tx, nil
}

// Clock places the pin on its first reading; the copier under test takes
// it as its Options.Clock.
func (p *ShadowPin) Clock() *HookedClock {
	return p.clock
}

// Wait blocks until the pin is in place and fails the test if placing it
// did not succeed.
func (p *ShadowPin) Wait(t *testing.T) {
	t.Helper()
	const pinDeadline = 15 * time.Second
	select {
	case <-p.pinned:
	case <-time.After(pinDeadline):
		t.Fatalf("the copier did not reach its first chunk within %s", pinDeadline)
	}
	require.NoError(t, p.err)
}

// Release waits for the pin and rolls it back, once.
func (p *ShadowPin) Release(t *testing.T) {
	t.Helper()
	p.Wait(t)
	assert.NoError(t, p.finish(func(tx pgx.Tx) error { return tx.Rollback(context.WithoutCancel(t.Context())) }))
}

// Commit waits for the pin and publishes the pinned row, once.
func (p *ShadowPin) Commit(t *testing.T) {
	t.Helper()
	p.Wait(t)
	require.NoError(t, p.finish(func(tx pgx.Tx) error { return tx.Commit(t.Context()) }))
}

// finish ends the pin's transaction with end, once. A pin that was never
// placed, or whose placing failed, has no transaction to end.
func (p *ShadowPin) finish(end func(pgx.Tx) error) error {
	var err error
	p.ended.Do(func() {
		if p.tx == nil {
			return
		}
		err = end(p.tx)
	})
	return err
}

// end is the pin's cleanup. It disarms a clock that was never read, so a
// later reading places nothing; otherwise it waits for the placing to
// finish, reports a placing that failed, and rolls back a pin still open.
func (p *ShadowPin) end(ctx context.Context) error {
	p.clock.once.Do(func() { close(p.pinned) })
	<-p.pinned
	if p.err != nil {
		return p.err
	}
	return p.finish(func(tx pgx.Tx) error { return tx.Rollback(ctx) })
}
