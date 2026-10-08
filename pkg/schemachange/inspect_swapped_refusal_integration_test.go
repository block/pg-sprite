//go:build integration || !unit

package schemachange_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// swappedTickets cuts over a table whose key is an ALWAYS identity column,
// so the swap recreates it on the live table under the source sequence's
// name and the inspection has an identity handoff to confirm.
func (f shadowFixture) swappedTickets(t *testing.T) staged {
	t.Helper()
	f.exec(t, `
		CREATE TABLE %s.tickets (
			id  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			qty integer NOT NULL
		)`)
	f.exec(t, `INSERT INTO %s.tickets (qty) SELECT g FROM generate_series(1, 100) g`)
	s := f.stage(t, "tickets", `ALTER TABLE %s.tickets ALTER COLUMN qty TYPE bigint`)
	_, err := f.cutover(t, s, schemachange.CutoverOptions{})
	require.NoError(t, err)
	return s
}

// A run that dies while its swap's COMMIT is still deciding on the server
// leaves that backend running: its connection is gone, but the server
// finishes the commit on its own. The resumed run's inspection must not
// answer "not swapped" from a catalog the commit has not reached yet,
// because moments later the shadow is live (LK-4); while that backend
// holds both relations exclusively the inspection refuses as ambiguous
// instead. Here the Cutover call is only the vehicle that leaves a COMMIT
// in flight; the inspection runs from the fixture's own pool, as a
// resumed run would.
func TestInspectSwappedDoesNotReportNotSwappedWhileTheSwapIsCommitting(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	write := f.slowCommit(t, 3*time.Second)
	s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)
	ready, err := f.gate(t, s)
	require.NoError(t, err)
	var lose atomic.Bool
	pool := f.unreachablePool(t, &lose, true)
	drain := func(ctx context.Context, tx pgx.Tx) error {
		lose.Store(true)
		return write(ctx, tx)
	}
	cutoverDone := make(chan struct{})
	go func() {
		defer close(cutoverDone)
		_, _ = schemachange.Cutover(t.Context(), pool, s.lock, ready, drain, schemachange.CutoverOptions{})
	}()
	const commitStarts = 10 * time.Second
	require.Eventually(t, func() bool {
		var committing int
		require.NoError(t, f.pool.QueryRow(t.Context(), `
			SELECT count(*) FROM pg_stat_activity
			WHERE state = 'active' AND query ILIKE 'commit%' AND pid <> pg_backend_pid()`).Scan(&committing))
		return committing > 0
	}, commitStarts, 20*time.Millisecond)

	_, inspectErr := schemachange.InspectSwapped(t.Context(), f.pool, s.lock, s.built.Proof(), schemachange.Options{})

	<-cutoverDone
	f.waitForCommitToFinish(t)
	require.Equal(t, s.built.ShadowOID(), f.relationOID(t, "orders"), "the server committed the swap")
	assert.NotErrorIs(t, inspectErr, schemachange.ErrNotSwapped, "a swap still committing is not a swap that never happened")
	assert.Equal(t, schemachange.CauseOutcomeAmbiguous, schemachange.RefusalCauseOf(inspectErr), "the outcome cannot be read while the swap's backend holds both relations")

	inspected, err := schemachange.InspectSwapped(t.Context(), f.pool, s.lock, s.built.Proof(), schemachange.Options{})
	require.NoError(t, err, "once the backend has let go the same call answers")
	assert.Equal(t, s.built.ShadowOID(), inspected.LiveOID())
}

// A retained source renamed away from its _old name still exists: the
// inspection does not report it as dropped — that would leave a full copy
// of the table the resume says it no longer has — and refuses, naming
// where it went, so an operator reads the catalog before anything is
// dropped (ST-6).
func TestInspectSwappedDoesNotReportARenamedOldTableAsDropped(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)
	swapped, err := f.cutover(t, s, schemachange.CutoverOptions{})
	require.NoError(t, err)
	f.exec(t, `ALTER TABLE %s.`+pgx.Identifier{swapped.OldTable()}.Sanitize()+` RENAME TO orders_retained`)

	_, err = schemachange.InspectSwapped(t.Context(), f.pool, s.lock, s.built.Proof(), schemachange.Options{})

	assert.NotErrorIs(t, err, schemachange.ErrOldTableNotFound, "the retained source is still there, as orders_retained")
	assert.Equal(t, schemachange.CauseRelationReplaced, schemachange.RefusalCauseOf(err))
	assert.True(t, f.relationExists(t, "orders_retained"), "the retained source is untouched")
}

// An inspection whose lock session no longer holds the table, because a
// rival engine now does, refuses rather than mint a proof (LK-1): the
// lock is confirmed from the inspection's own transaction, not taken on
// the session's word.
func TestInspectSwappedRefusesALockHeldByAnotherBackend(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	own, err := dbconn.AcquireTableLock(t.Context(), f.cfg, f.schema, "orders")
	require.NoError(t, err)
	s := f.stageUnder(t, own, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)
	ready, err := f.gate(t, s)
	require.NoError(t, err)
	_, err = schemachange.Cutover(t.Context(), f.pool, own, ready, nil, schemachange.CutoverOptions{})
	require.NoError(t, err)
	require.NoError(t, own.Release(context.WithoutCancel(t.Context())))
	stale := f.goneLock(t, "orders")
	rival := f.lock(t, "orders")

	_, err = schemachange.InspectSwapped(t.Context(), f.pool, stale, s.built.Proof(), schemachange.Options{})

	assert.Equal(t, schemachange.CauseLockHeldElsewhere, schemachange.RefusalCauseOf(err))
	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation)
	assert.NoError(t, rival.Err(), "the rival's lock is untouched")
}

// An identity the swap recreated as GENERATED ALWAYS that the live table
// now generates BY DEFAULT is not the table the swap left: the inspection
// refuses it as a swap mismatch.
func TestInspectSwappedRefusesAnIdentityGeneratedDifferently(t *testing.T) {
	f := newShadowFixture(t)
	s := f.swappedTickets(t)
	f.exec(t, `ALTER TABLE %s.tickets ALTER COLUMN id SET GENERATED BY DEFAULT`)

	_, err := schemachange.InspectSwapped(t.Context(), f.pool, s.lock, s.built.Proof(), schemachange.Options{})

	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation)
	assert.Equal(t, schemachange.CauseSwapMismatch, schemachange.RefusalCauseOf(err))
}

// An identity the swap recreated under the source sequence's name that now
// draws from a sequence of another name is not the table the swap left:
// the inspection refuses it as a swap mismatch.
func TestInspectSwappedRefusesAnIdentityDrawingFromAnotherSequence(t *testing.T) {
	f := newShadowFixture(t)
	s := f.swappedTickets(t)
	f.exec(t, `ALTER SEQUENCE %s.tickets_id_seq RENAME TO tickets_id_seq_elsewhere`)

	_, err := schemachange.InspectSwapped(t.Context(), f.pool, s.lock, s.built.Proof(), schemachange.Options{})

	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation)
	assert.Equal(t, schemachange.CauseSwapMismatch, schemachange.RefusalCauseOf(err))
}

// A live table whose owner changed since the swap is not the table the
// swap left: the proof would name the recorded role as the owner and
// DropOldTable would run as it, so the inspection reads both relations'
// owners from the catalog and refuses the mismatch (ST-6).
func TestInspectSwappedRefusesALiveTableOwnedByAnotherRole(t *testing.T) {
	f, role := newShadowFixtureWithRole(t)
	s := f.swappedTickets(t)
	f.exec(t, `ALTER TABLE %s.tickets OWNER TO `+pgx.Identifier{role}.Sanitize())

	_, err := schemachange.InspectSwapped(t.Context(), f.pool, s.lock, s.built.Proof(), schemachange.Options{})

	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation)
	assert.Equal(t, schemachange.CauseSwapMismatch, schemachange.RefusalCauseOf(err))
}

// An old table whose owner changed since the swap is refused the same way:
// the drop that consumes the proof runs as the recorded role, and the
// inspection does not mint a proof that role could not act on.
func TestInspectSwappedRefusesAnOldTableOwnedByAnotherRole(t *testing.T) {
	f, role := newShadowFixtureWithRole(t)
	s := f.swappedTickets(t)
	f.exec(t, `ALTER TABLE %s.`+pgx.Identifier{schemachange.OldName(f.schema, "tickets")}.Sanitize()+` OWNER TO `+pgx.Identifier{role}.Sanitize())

	_, err := schemachange.InspectSwapped(t.Context(), f.pool, s.lock, s.built.Proof(), schemachange.Options{})

	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation)
	assert.Equal(t, schemachange.CauseSwapMismatch, schemachange.RefusalCauseOf(err))
}
