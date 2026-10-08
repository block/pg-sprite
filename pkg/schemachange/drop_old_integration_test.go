//go:build integration || !unit

package schemachange_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// DropOldTable runs only under the table's lock session, like every other
// operation on the table (LK-1): without one it refuses and drops nothing.
func TestDropOldTableRefusesAMissingLock(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)
	swapped, err := f.cutover(t, s, schemachange.CutoverOptions{})
	require.NoError(t, err)

	err = schemachange.DropOldTable(t.Context(), f.pool, nil, swapped, schemachange.Options{})

	assert.Equal(t, schemachange.CauseLockUnproven, schemachange.RefusalCauseOf(err))
	assert.True(t, f.relationExists(t, swapped.OldTable()), "a refused drop removes nothing")
}

// The old-table drop is without CASCADE: a view that came to depend on the
// retained table after the swap makes the drop fail with the server's
// dependent_objects_still_exist, and both the view and the old table stay
// (D9). Removing the dependent is the operator's decision, not pg-sprite's.
func TestDropOldTableKeepsAnObjectThatDependsOnTheOldTable(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)
	swapped, err := f.cutover(t, s, schemachange.CutoverOptions{})
	require.NoError(t, err)
	f.exec(t, `CREATE VIEW %s.old_orders_v AS SELECT id FROM %s.`+swapped.OldTable())

	err = schemachange.DropOldTable(t.Context(), f.pool, s.lock, swapped, schemachange.Options{})

	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "2BP01", pgErr.Code, "dependent_objects_still_exist: the drop was not CASCADE")
	assert.True(t, f.relationExists(t, "old_orders_v"), "the dependent view is untouched")
	assert.True(t, f.relationExists(t, swapped.OldTable()), "the old table is kept for the operator")
}

// The drop confirms from its own transaction that the lock session's
// backend holds the table lock: a session whose backend is gone while a
// second instance holds the lock drops nothing (LK-1).
func TestDropOldTableRefusesALockHeldByAnotherBackend(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	own, err := dbconn.AcquireTableLock(t.Context(), f.cfg, f.schema, "orders")
	require.NoError(t, err)
	s := f.stageUnder(t, own, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)
	ready, err := f.gate(t, s)
	require.NoError(t, err)
	swapped, err := schemachange.Cutover(t.Context(), f.pool, own, ready, nil, schemachange.CutoverOptions{})
	require.NoError(t, err)
	require.NoError(t, own.Release(context.WithoutCancel(t.Context())))
	stale := f.goneLock(t, "orders")
	rival := f.lock(t, "orders")

	err = schemachange.DropOldTable(t.Context(), f.pool, stale, swapped, schemachange.Options{})

	assert.Equal(t, schemachange.CauseLockHeldElsewhere, schemachange.RefusalCauseOf(err))
	assert.True(t, errors.Is(err, schemachange.ErrInvariantViolation))
	assert.True(t, f.relationExists(t, swapped.OldTable()), "a refused drop removes nothing")
	assert.NoError(t, rival.Err(), "the rival's lock is untouched")
}

// The relation the drop proved by OID is the relation it drops: the drop
// locks whatever bears the _old name before it reads the OID, so a rename
// that moves the retained source away from the name and puts another
// table there while the drop waits is refused once it commits, not
// followed to the newcomer (ST-6).
func TestDropOldTableDropsOnlyTheRelationItProved(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)
	swapped, err := f.cutover(t, s, schemachange.CutoverOptions{})
	require.NoError(t, err)
	old := pgx.Identifier{f.schema, swapped.OldTable()}.Sanitize()

	rival, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		// Redundant safety closer: the rival commits below, so this
		// returns the guaranteed ErrTxClosed.
		_ = rival.Rollback(context.WithoutCancel(t.Context()))
	})
	_, err = rival.Exec(t.Context(), `ALTER TABLE `+old+` RENAME TO orders_retained`)
	require.NoError(t, err)
	_, err = rival.Exec(t.Context(), `CREATE TABLE `+old+` (impostor integer)`)
	require.NoError(t, err)

	dropped := make(chan error, 1)
	go func() {
		dropped <- schemachange.DropOldTable(t.Context(), f.pool, s.lock, swapped, schemachange.Options{})
	}()
	const dropWaits = 10 * time.Second
	require.Eventually(t, func() bool { return f.backendWaitingOnLock(t, "%"+swapped.OldTable()+"%") }, dropWaits, 20*time.Millisecond)
	require.NoError(t, rival.Commit(t.Context()))
	err = <-dropped

	assert.Equal(t, schemachange.CauseRelationReplaced, schemachange.RefusalCauseOf(err))
	assert.True(t, f.relationExists(t, swapped.OldTable()), "the table that took the _old name is not dropped")
	assert.True(t, f.relationExists(t, "orders_retained"), "nor is the retained source")
}
