package schemachange_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// Every shadow operation refuses to run without the per-table lock session:
// the lock is what keeps a second engine instance off the same table, so
// a missing session is an invariant violation, not a default.
func TestShadowOperationsRequireATableLock(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.widgets (
			id bigint PRIMARY KEY,
			qty integer NOT NULL
		)`)
	target := f.prove(t, "widgets")
	alter := f.alter(t, `ALTER TABLE %s.widgets ALTER COLUMN qty TYPE bigint`)

	_, err := schemachange.BuildShadow(t.Context(), f.pool, nil, target, alter, schemachange.Options{})
	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation, "build")
	assert.False(t, f.relationExists(t, schemachange.ShadowName(f.schema, "widgets")), "a refused build creates nothing")

	err = schemachange.DropShadow(t.Context(), f.pool, nil, target, schemachange.Options{})
	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation, "drop")

	_, err = schemachange.InspectShadow(t.Context(), f.pool, nil, target, schemachange.Options{})
	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation, "inspect")
}

// A lock on some other table is not a lock on the proven one: the session
// and the proof must name the same table.
func TestShadowOperationsRefuseALockForAnotherTable(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.widgets (
			id bigint PRIMARY KEY,
			qty integer NOT NULL
		)`)
	f.exec(t, `
		CREATE TABLE %s.gadgets (
			id bigint PRIMARY KEY
		)`)
	target := f.prove(t, "widgets")
	alter := f.alter(t, `ALTER TABLE %s.widgets ALTER COLUMN qty TYPE bigint`)
	otherLock := f.lock(t, "gadgets")

	_, err := schemachange.BuildShadow(t.Context(), f.pool, otherLock, target, alter, schemachange.Options{})
	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation, "build")
	assert.False(t, f.relationExists(t, schemachange.ShadowName(f.schema, "widgets")), "a refused build creates nothing")

	err = schemachange.DropShadow(t.Context(), f.pool, otherLock, target, schemachange.Options{})
	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation, "drop")

	_, err = schemachange.InspectShadow(t.Context(), f.pool, otherLock, target, schemachange.Options{})
	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation, "inspect")
}

// A caller that cancels its own context, with a cause of its own choosing,
// gets that cancellation back: the lock session still holds the table, so
// no operation reports a lock loss that did not happen.
func TestShadowOperationsReportACallerCancellationAsTheCallersOwn(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.widgets (
			id bigint PRIMARY KEY,
			qty integer NOT NULL
		)`)
	lock := f.lock(t, "widgets")
	target := f.prove(t, "widgets")
	alter := f.alter(t, `ALTER TABLE %s.widgets ALTER COLUMN qty TYPE bigint`)
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(errors.New("operator aborted the change"))

	_, err := schemachange.BuildShadow(ctx, f.pool, lock, target, alter, schemachange.Options{})
	assert.ErrorIs(t, err, context.Canceled, "build")
	assert.NotErrorIs(t, err, schemachange.ErrInvariantViolation, "build")

	err = schemachange.DropShadow(ctx, f.pool, lock, target, schemachange.Options{})
	assert.ErrorIs(t, err, context.Canceled, "drop")
	assert.NotErrorIs(t, err, schemachange.ErrInvariantViolation, "drop")

	_, err = schemachange.InspectShadow(ctx, f.pool, lock, target, schemachange.Options{})
	assert.ErrorIs(t, err, context.Canceled, "inspect")
	assert.NotErrorIs(t, err, schemachange.ErrInvariantViolation, "inspect")

	assert.NoError(t, lock.Err(), "the lock session held the table throughout")
}

// The build transaction confirms from its own connection that the lock
// session's backend still holds the table. A lock session whose backend
// is already gone — before its keepalive has noticed — is refused at that
// check, so the build never trusts a lock the server no longer grants.
func TestBuildShadowRefusesWhenTheLockSessionIsGone(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.widgets (
			id bigint PRIMARY KEY,
			qty integer NOT NULL
		)`)
	target := f.prove(t, "widgets")
	alter := f.alter(t, `ALTER TABLE %s.widgets ALTER COLUMN qty TYPE bigint`)
	lock := f.goneLock(t, "widgets")

	_, err := schemachange.BuildShadow(t.Context(), f.pool, lock, target, alter, schemachange.Options{})
	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation)
	assert.False(t, f.relationExists(t, schemachange.ShadowName(f.schema, "widgets")), "a refused build creates nothing")
}

// Drop and inspect make the same in-transaction confirmation as the build:
// with the lock session's backend gone, neither trusts the session, the
// shadow an earlier build left is neither dropped nor reported as a proof.
func TestDropAndInspectShadowRefuseWhenTheLockSessionIsGone(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.widgets (
			id bigint PRIMARY KEY,
			qty integer NOT NULL
		)`)
	target := f.prove(t, "widgets")
	buildLock, err := dbconn.AcquireTableLock(t.Context(), f.cfg, f.schema, "widgets")
	require.NoError(t, err)
	_, err = schemachange.BuildShadow(t.Context(), f.pool, buildLock, target, f.alter(t, `ALTER TABLE %s.widgets ALTER COLUMN qty TYPE bigint`), schemachange.Options{})
	require.NoError(t, err)
	require.NoError(t, buildLock.Release(t.Context()))
	shadow := schemachange.ShadowName(f.schema, "widgets")
	lock := f.goneLock(t, "widgets")

	err = schemachange.DropShadow(t.Context(), f.pool, lock, target, schemachange.Options{})
	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation, "drop")
	assert.True(t, f.relationExists(t, shadow), "a refused drop removes nothing")

	_, err = schemachange.InspectShadow(t.Context(), f.pool, lock, target, schemachange.Options{})
	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation, "inspect")
}

// The in-transaction confirmation compares backends, not merely that some
// session holds the table. Once this instance's lock session is gone, a
// second instance can take the same lock before the first's keepalive
// notices; the first must not keep working under a lock the server now
// grants to someone else.
func TestShadowOperationsRefuseALockHeldByAnotherBackend(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.widgets (
			id bigint PRIMARY KEY,
			qty integer NOT NULL
		)`)
	target := f.prove(t, "widgets")
	buildLock, err := dbconn.AcquireTableLock(t.Context(), f.cfg, f.schema, "widgets")
	require.NoError(t, err)
	_, err = schemachange.BuildShadow(t.Context(), f.pool, buildLock, target, f.alter(t, `ALTER TABLE %s.widgets ALTER COLUMN qty TYPE bigint`), schemachange.Options{})
	require.NoError(t, err)
	require.NoError(t, buildLock.Release(t.Context()))
	shadow := schemachange.ShadowName(f.schema, "widgets")

	stale := f.goneLock(t, "widgets")
	rival := f.lock(t, "widgets")
	require.NotEqual(t, stale.BackendPID(), rival.BackendPID())

	err = schemachange.DropShadow(t.Context(), f.pool, stale, target, schemachange.Options{})
	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation, "drop")
	assert.True(t, f.relationExists(t, shadow), "a refused drop removes nothing")

	_, err = schemachange.InspectShadow(t.Context(), f.pool, stale, target, schemachange.Options{})
	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation, "inspect")

	_, err = schemachange.BuildShadow(t.Context(), f.pool, stale, target, f.alter(t, `ALTER TABLE %s.widgets ALTER COLUMN qty TYPE bigint`), schemachange.Options{})
	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation, "build")
	assert.NoError(t, rival.Err(), "the rival's lock is untouched")
}

// A lock session that has already reported loss is refused before any
// connection is opened: the error names the session's loss and nothing
// else, because no statement ran to fail under the cancelled context.
func TestShadowOperationsRefuseALockSessionThatReportedLoss(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.widgets (
			id bigint PRIMARY KEY,
			qty integer NOT NULL
		)`)
	target := f.prove(t, "widgets")
	alter := f.alter(t, `ALTER TABLE %s.widgets ALTER COLUMN qty TYPE bigint`)
	lock := f.lock(t, "widgets", dbconn.WithTableLockKeepalive(100*time.Millisecond))
	f.terminateBackend(t, lock.BackendPID())
	const lockLossDeadline = 10 * time.Second
	select {
	case <-lock.Done():
	case <-time.After(lockLossDeadline):
		t.Fatalf("lock session did not report loss within %s", lockLossDeadline)
	}
	require.Error(t, lock.Err())

	_, err := schemachange.BuildShadow(t.Context(), f.pool, lock, target, alter, schemachange.Options{})
	assertRefusedBeforeAnyStatement(t, err, lock, "build")
	assert.False(t, f.relationExists(t, schemachange.ShadowName(f.schema, "widgets")), "a refused build creates nothing")

	err = schemachange.DropShadow(t.Context(), f.pool, lock, target, schemachange.Options{})
	assertRefusedBeforeAnyStatement(t, err, lock, "drop")

	_, err = schemachange.InspectShadow(t.Context(), f.pool, lock, target, schemachange.Options{})
	assertRefusedBeforeAnyStatement(t, err, lock, "inspect")
}

// assertRefusedBeforeAnyStatement checks that err is the pre-connection
// LK-1 refusal for a session that reported loss: it wraps the session's
// own error and not the context cancellation a statement run under the
// dead session's Bind context would have added.
func assertRefusedBeforeAnyStatement(t *testing.T, err error, lock *dbconn.TableLockSession, op string) {
	t.Helper()
	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation, op)
	assert.ErrorIs(t, err, lock.Err(), op)
	assert.NotErrorIs(t, err, context.Canceled, op)
}

// A drop runs under the lock session's Bind context too: losing the lock
// while DROP TABLE is parked behind another session's lock on the shadow
// cancels the statement, and the shadow is left in place.
func TestDropShadowAbortsWhenTheLockIsLostMidDrop(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.widgets (
			id bigint PRIMARY KEY,
			qty integer NOT NULL
		)`)
	target := f.prove(t, "widgets")
	buildLock, err := dbconn.AcquireTableLock(t.Context(), f.cfg, f.schema, "widgets")
	require.NoError(t, err)
	_, err = schemachange.BuildShadow(t.Context(), f.pool, buildLock, target, f.alter(t, `ALTER TABLE %s.widgets ALTER COLUMN qty TYPE bigint`), schemachange.Options{})
	require.NoError(t, err)
	require.NoError(t, buildLock.Release(t.Context()))
	shadow := schemachange.ShadowName(f.schema, "widgets")
	lock, err := dbconn.AcquireTableLock(t.Context(), f.cfg, f.schema, "widgets",
		dbconn.WithTableLockKeepalive(100*time.Millisecond))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.Error(t, lock.Release(context.WithoutCancel(t.Context())))
	})

	blocker, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, blocker.Rollback(context.WithoutCancel(t.Context())))
	})
	_, err = blocker.Exec(t.Context(), fmt.Sprintf(`LOCK TABLE %s IN ACCESS EXCLUSIVE MODE`, pgx.Identifier{f.schema, shadow}.Sanitize()))
	require.NoError(t, err)

	const dropLockTimeout = 30 * time.Second
	results := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() {
		results <- schemachange.DropShadow(t.Context(), f.pool, lock, target, schemachange.Options{LockTimeout: dropLockTimeout})
	})
	t.Cleanup(wg.Wait)

	const dropParkedDeadline = 15 * time.Second
	require.Eventually(t, func() bool {
		return f.backendWaitingOnLock(t, "DROP TABLE%")
	}, dropParkedDeadline, 50*time.Millisecond, "the drop should be waiting for the shadow's lock")

	f.terminateBackend(t, lock.BackendPID())

	const lockLossDeadline = 10 * time.Second
	select {
	case err := <-results:
		assert.ErrorIs(t, err, schemachange.ErrInvariantViolation)
		assert.ErrorIs(t, err, lock.Err(), "the loss the session reported is the cause")
	case <-time.After(lockLossDeadline):
		t.Fatalf("drop did not abort within %s of lock loss", lockLossDeadline)
	}
	assert.True(t, f.relationExists(t, shadow), "an aborted drop leaves the shadow in place")
}

// goneLock acquires the table lock and then terminates the session's
// backend, so the server no longer grants the lock while the session still
// believes it holds it: the default keepalive is long enough that only an
// in-transaction confirmation can catch the loss. The cleanup waits for the
// session to notice on its own before releasing.
func (f shadowFixture) goneLock(t *testing.T, table string) *dbconn.TableLockSession {
	t.Helper()
	lock, err := dbconn.AcquireTableLock(t.Context(), f.cfg, f.schema, table)
	require.NoError(t, err)
	t.Cleanup(func() {
		const lockLossDeadline = 30 * time.Second
		select {
		case <-lock.Done():
		case <-time.After(lockLossDeadline):
			t.Errorf("lock session did not report loss within %s", lockLossDeadline)
		}
		assert.Error(t, lock.Release(context.WithoutCancel(t.Context())))
	})
	f.terminateBackend(t, lock.BackendPID())
	require.NoError(t, lock.Err(), "the keepalive has not yet noticed the loss; the in-transaction check must")
	return lock
}

// A build runs under the lock session's Bind context: losing the lock while
// a statement is in flight cancels that statement and the build reports the
// loss as an invariant violation, leaving no shadow behind. The build is
// parked on the source's ACCESS EXCLUSIVE lock so the loss lands mid-build.
func TestBuildShadowAbortsWhenTheLockIsLostMidBuild(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.widgets (
			id bigint PRIMARY KEY,
			qty integer NOT NULL
		)`)
	target := f.prove(t, "widgets")
	alter := f.alter(t, `ALTER TABLE %s.widgets ALTER COLUMN qty TYPE bigint`)
	lock, err := dbconn.AcquireTableLock(t.Context(), f.cfg, f.schema, "widgets",
		dbconn.WithTableLockKeepalive(100*time.Millisecond))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.Error(t, lock.Release(context.WithoutCancel(t.Context())))
	})

	blocker, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, blocker.Rollback(context.WithoutCancel(t.Context())))
	})
	_, err = blocker.Exec(t.Context(), fmt.Sprintf(`LOCK TABLE %s IN ACCESS EXCLUSIVE MODE`, pgx.Identifier{f.schema, "widgets"}.Sanitize()))
	require.NoError(t, err)

	const buildLockTimeout = 30 * time.Second
	results := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() {
		_, err := schemachange.BuildShadow(t.Context(), f.pool, lock, target, alter, schemachange.Options{LockTimeout: buildLockTimeout})
		results <- err
	})
	t.Cleanup(wg.Wait)

	const buildParkedDeadline = 15 * time.Second
	require.Eventually(t, func() bool {
		return f.backendWaitingOnLock(t, "CREATE TABLE%")
	}, buildParkedDeadline, 50*time.Millisecond, "the build should be waiting for the source table lock")

	f.terminateBackend(t, lock.BackendPID())

	const lockLossDeadline = 10 * time.Second
	select {
	case err := <-results:
		assert.ErrorIs(t, err, schemachange.ErrInvariantViolation)
		assert.ErrorIs(t, err, lock.Err(), "the loss the session reported is the cause")
	case <-time.After(lockLossDeadline):
		t.Fatalf("build did not abort within %s of lock loss", lockLossDeadline)
	}
	assert.False(t, f.relationExists(t, schemachange.ShadowName(f.schema, "widgets")), "an aborted build leaves no shadow")
}

// terminateBackend ends one server backend and waits until the server no
// longer lists it, so its locks are provably released.
func (f shadowFixture) terminateBackend(t *testing.T, pid uint32) {
	t.Helper()
	var terminated bool
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT pg_terminate_backend($1)`, pid).Scan(&terminated))
	require.True(t, terminated)
	const backendExitDeadline = 10 * time.Second
	require.Eventually(t, func() bool {
		var alive bool
		require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1)`, pid).Scan(&alive))
		return !alive
	}, backendExitDeadline, 20*time.Millisecond, "terminated backend should leave pg_stat_activity")
}

// backendWaitingOnLock reports whether some backend is parked on a heavyweight
// lock while running a statement matching pattern.
func (f shadowFixture) backendWaitingOnLock(t *testing.T, pattern string) bool {
	t.Helper()
	var waiting bool
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query LIKE $1)`, pattern).Scan(&waiting))
	return waiting
}
