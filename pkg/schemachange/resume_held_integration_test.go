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

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// resumeHeldAlter is the statement every stopped run here was carrying:
//
//	ALTER TABLE orders DROP COLUMN note
//
// on the orders table createOrders makes, so its shadow has the columns
// id, qty, and sku.
const resumeHeldAlter = `ALTER TABLE %s.orders DROP COLUMN note`

// MeasureResumeHeld counts only what a run the next run resumes holds on
// the volume. Every case below leaves orders in one state a stopped run,
// or something wearing its names, can leave, and pins the exact figure:
// the shadow's size as the server reports it when the row is one the run
// resumes and an ordinary table wears the shadow's name, and nothing for
// every reading that does not prove space the resume keeps, so the disk
// check falls back to a fresh run's requirement. The cases that plant the
// route's slot give it retained WAL, so a slot read the measure must not
// make would show as a figure.
func TestMeasureResumeHeldOverTheRowStates(t *testing.T) {
	serverURL := testutil.StartPostgresWithSettings(t, "wal_level=logical")
	cases := []struct {
		name       string
		decodesWAL bool
		// leave leaves orders in the case's state and returns what the
		// measure must report for it.
		leave func(t *testing.T, f runnerFixture) preflight.CopySwapHeld
	}{
		{
			// A fresh run refuses a relation no row records (ST-2), and
			// is asked for the whole requirement, so the table under the
			// shadow's name counts nothing.
			name: "no row counts nothing, even with a table under the shadow's name",
			leave: func(t *testing.T, f runnerFixture) preflight.CopySwapHeld {
				require.NoError(t, f.store.Ensure(t.Context()))
				f.leaveTableUnderShadowName(t)
				return preflight.CopySwapHeld{}
			},
		},
		{
			// A done row is replaced by a fresh run, which builds its
			// own shadow.
			name: "a done row counts nothing, even with a table under the shadow's name",
			leave: func(t *testing.T, f runnerFixture) preflight.CopySwapHeld {
				_, err := f.run(t, "orders", resumeHeldAlter, schemachange.RunOptions{})
				require.NoError(t, err)
				require.Equal(t, checkpoint.PhaseDone, f.inspectRow(t, "orders").Phase)
				f.leaveTableUnderShadowName(t)
				return preflight.CopySwapHeld{}
			},
		},
		{
			name: "a copying row counts its shadow",
			leave: func(t *testing.T, f runnerFixture) preflight.CopySwapHeld {
				shadow := f.leaveCopyingRow(t, func(*checkpoint.Checkpoint) {})
				return preflight.CopySwapHeld{ShadowBytes: f.totalRelationSize(t, shadow)}
			},
		},
		{
			name: "a copying row whose shadow is gone counts nothing",
			leave: func(t *testing.T, f runnerFixture) preflight.CopySwapHeld {
				shadow := f.leaveCopyingRow(t, func(*checkpoint.Checkpoint) {})
				f.exec(t, `DROP TABLE %s.`+shadow)
				return preflight.CopySwapHeld{}
			},
		},
		{
			// A row in another format is one this engine cannot read; the
			// run itself refuses it, so it is no resume to discount.
			name: "an incompatible row counts nothing, though its shadow stands",
			leave: func(t *testing.T, f runnerFixture) preflight.CopySwapHeld {
				f.leaveCopyingRow(t, func(*checkpoint.Checkpoint) {})
				_, err := f.pool.Exec(t.Context(), `
					UPDATE pgsprite.pgsprite_checkpoint
					SET format_version = format_version + 1
					WHERE schema_name = $1
					  AND table_name = 'orders'`, f.schema)
				require.NoError(t, err)
				_, err = f.store.Inspect(t.Context(), f.schema, "orders")
				require.ErrorAs(t, err, new(*checkpoint.IncompatibleError), "the row is one this engine cannot read")
				return preflight.CopySwapHeld{}
			},
		},
		{
			// Only an ordinary table is a shadow. A materialized view has
			// storage of its own, so it would show as a figure if it were
			// counted; a plain view has none and reads zero either way.
			// It selects from no table, so orders keeps the shape the
			// route admits (a view on orders is refused as a dependent).
			name: "a materialized view under the shadow's name counts nothing",
			leave: func(t *testing.T, f runnerFixture) preflight.CopySwapHeld {
				shadow := f.leaveCopyingRow(t, func(*checkpoint.Checkpoint) {})
				f.exec(t, `DROP TABLE %s.`+shadow)
				f.exec(t, `
					CREATE MATERIALIZED VIEW %s.`+shadow+` AS
					SELECT g AS id, repeat('x', 100) AS payload
					FROM generate_series(1, 2500) AS g`)
				require.Positive(t, f.totalRelationSize(t, shadow), "the materialized view has storage")
				return preflight.CopySwapHeld{}
			},
		},
		{
			// A run under --block-writes decodes no WAL, so even the
			// route's slot standing with WAL retained is not its own.
			name: "a quiesced run counts its shadow and no slot, with the route's slot standing",
			leave: func(t *testing.T, f runnerFixture) preflight.CopySwapHeld {
				slot := f.copySwapShape(t).DecodingName()
				shadow := f.leaveCopyingRow(t, func(row *checkpoint.Checkpoint) {
					row.SlotName = slot
					row.PublicationName = slot
				})
				f.plantSlotRetainingWAL(t, slot)
				return preflight.CopySwapHeld{ShadowBytes: f.totalRelationSize(t, shadow)}
			},
		},
		{
			// A live run's row names its slot from the save that made it;
			// a row that names none has no slot of its own to keep.
			name:       "a live run's row that names no slot counts no slot, with the route's slot standing",
			decodesWAL: true,
			leave: func(t *testing.T, f runnerFixture) preflight.CopySwapHeld {
				shadow := f.leaveCopyingRow(t, func(*checkpoint.Checkpoint) {})
				f.plantSlotRetainingWAL(t, f.copySwapShape(t).DecodingName())
				return preflight.CopySwapHeld{ShadowBytes: f.totalRelationSize(t, shadow)}
			},
		},
		{
			// The swap committed: the shadow already bears the table's
			// name, and the resume only finishes the swap.
			name: "a cutover row whose swap committed reports the committed swap and measures nothing",
			leave: func(t *testing.T, f runnerFixture) preflight.CopySwapHeld {
				f.committedCutoverRow(t)
				return preflight.CopySwapHeld{SwapCommitted: true}
			},
		},
		{
			// The same committed swap for a live run whose row names the
			// route's slot, with that slot retaining WAL: nothing is
			// measured, so the slot's retained WAL is not reported either.
			name:       "a committed swap of a live run measures no slot, with the row naming the route's slot",
			decodesWAL: true,
			leave: func(t *testing.T, f runnerFixture) preflight.CopySwapHeld {
				f.committedCutoverRow(t)
				slot := f.copySwapShape(t).DecodingName()
				lock := f.lock(t, "orders")
				row, err := f.store.Inspect(t.Context(), f.schema, "orders")
				require.NoError(t, err)
				row.SlotName = slot
				row.PublicationName = slot
				require.NoError(t, f.store.Save(t.Context(), lock, row))
				require.NoError(t, lock.Release(t.Context()))
				f.plantSlotRetainingWAL(t, slot)
				return preflight.CopySwapHeld{SwapCommitted: true}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newRunnerFixtureOn(t, serverURL)
			f.createOrders(t)
			want := c.leave(t, f)
			held, err := schemachange.MeasureResumeHeld(t.Context(), f.pool, f.store, f.copySwapShape(t), c.decodesWAL)
			require.NoError(t, err)
			assert.Equal(t, want, held)
		})
	}

	// The control for the two slot cases above: the same planted slot,
	// named by a live run's row, is counted, so their zero is the
	// measure declining to count it, not a slot it could not see.
	t.Run("a live run's row that names the route's slot counts its retained WAL", func(t *testing.T) {
		f := newRunnerFixtureOn(t, serverURL)
		f.createOrders(t)
		shape := f.copySwapShape(t)
		slot := shape.DecodingName()
		shadow := f.leaveCopyingRow(t, func(row *checkpoint.Checkpoint) {
			row.SlotName = slot
			row.PublicationName = slot
		})
		f.plantSlotRetainingWAL(t, slot)
		retained := f.slotRetained(t, slot)

		held, err := schemachange.MeasureResumeHeld(t.Context(), f.pool, f.store, shape, true)
		require.NoError(t, err)
		assert.False(t, held.SwapCommitted)
		assert.Equal(t, f.totalRelationSize(t, shadow), held.ShadowBytes)
		assert.GreaterOrEqual(t, held.RetainedWALBytes, retained, "the slot's retained WAL, which only grows")
	})
}

// The shadow is looked for on the shape's own database. A pool on another
// database of the same server finds no relation of the shape's: a table
// there under the same schema and shadow name is another database's, and
// counts nothing.
func TestMeasureResumeHeldCountsNoTableOfAnotherDatabase(t *testing.T) {
	serverURL := testutil.StartPostgres(t)
	f := newRunnerFixtureOn(t, testutil.NewDatabase(t, serverURL))
	f.createOrders(t)
	shadow := f.leaveCopyingRow(t, func(*checkpoint.Checkpoint) {})
	shape := f.copySwapShape(t)

	other, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.NewDatabase(t, serverURL)})
	require.NoError(t, err)
	t.Cleanup(other.Close)
	qualifiedShadow := pgx.Identifier{f.schema, shadow}.Sanitize()
	for _, ddl := range []string{
		`CREATE SCHEMA ` + pgx.Identifier{f.schema}.Sanitize(),
		`
		CREATE TABLE ` + qualifiedShadow + ` AS
		SELECT g AS id, repeat('x', 100) AS payload
		FROM generate_series(1, 2500) AS g`,
	} {
		_, err := other.Exec(t.Context(), ddl)
		require.NoError(t, err)
	}
	var otherSize int64
	require.NoError(t, other.QueryRow(t.Context(),
		`SELECT pg_total_relation_size($1::regclass)`, qualifiedShadow).Scan(&otherSize))
	require.Positive(t, otherSize, "the other database's table has storage")

	held, err := schemachange.MeasureResumeHeld(t.Context(), other, f.store, shape, false)
	require.NoError(t, err)
	assert.Equal(t, preflight.CopySwapHeld{}, held)
}

// A shadow size read that fails is returned, never read as nothing held:
// a preflight that counted it as zero would ask a resume for the whole
// requirement on a figure it never read. Here a session holds the shadow
// ACCESS EXCLUSIVE, and the measure's pool gives up on that lock after
// its lock_timeout.
func TestMeasureResumeHeldReturnsAShadowReadThatFails(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	shadow := f.leaveCopyingRow(t, func(*checkpoint.Checkpoint) {})
	shape := f.copySwapShape(t)

	holder, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, holder.Rollback(context.WithoutCancel(t.Context())))
	})
	_, err = holder.Exec(t.Context(), `LOCK TABLE `+pgx.Identifier{f.schema, shadow}.Sanitize()+` IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)

	impatient, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: f.cfg.URL, LockTimeout: 100 * time.Millisecond})
	require.NoError(t, err)
	t.Cleanup(impatient.Close)

	_, err = schemachange.MeasureResumeHeld(t.Context(), impatient, f.store, shape, false)
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	require.True(t, ok, "the failed read is returned: %v", err)
	assert.Equal(t, "55P03", pgErr.Code, "lock_not_available")
}

// leaveCopyingRow leaves orders as a run that stopped partway through its
// copy does — a copying row for resumeHeldAlter, with mutate applied, and
// its shadow — with every source row copied into the shadow so its size
// is well above an empty table's, then vacuums the shadow so autovacuum
// does not change its size while a test reads it. It returns the
// shadow's name.
func (f runnerFixture) leaveCopyingRow(t *testing.T, mutate func(*checkpoint.Checkpoint)) string {
	t.Helper()
	built := f.leaveStoppedRun(t, resumeHeldAlter, mutate)
	shadow := built.ShadowTable()
	f.exec(t, `
		INSERT INTO %s.`+shadow+` (id, qty, sku)
		SELECT id, qty, sku
		FROM %s.orders`)
	f.exec(t, `VACUUM (ANALYZE) %s.`+shadow)
	return shadow
}

// leaveTableUnderShadowName creates an ordinary table, holding rows,
// under the name the route derives for orders' shadow.
func (f runnerFixture) leaveTableUnderShadowName(t *testing.T) {
	t.Helper()
	shadow := schemachange.ShadowName(f.schema, "orders")
	f.exec(t, `
		CREATE TABLE %s.`+shadow+` AS
		SELECT id, qty, sku
		FROM %s.orders`)
	require.Positive(t, f.totalRelationSize(t, shadow), "the table has storage")
}

// copySwapShape is the shape proof the route's preflight hands
// MeasureResumeHeld for orders.
func (f runnerFixture) copySwapShape(t *testing.T) preflight.CopySwapShape {
	t.Helper()
	role, err := preflight.CheckPrivileges(t.Context(), f.pool, f.schema, "orders",
		preflight.Requirement{Tier: preflight.TierCopyAndSwap})
	require.NoError(t, err)
	shape, err := preflight.CheckCopySwapShape(t.Context(), f.pool, f.schema, "orders", role)
	require.NoError(t, err)
	return shape
}

// totalRelationSize is the server's total size of a relation in the
// fixture's schema: heap, indexes, and TOAST.
func (f runnerFixture) totalRelationSize(t *testing.T, name string) int64 {
	t.Helper()
	var size int64
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT pg_total_relation_size($1::regclass)`,
		pgx.Identifier{f.schema, name}.Sanitize()).Scan(&size))
	return size
}

// plantSlotRetainingWAL creates a logical slot of name, as the route's
// slot would stand, and writes WAL past its restart position so the slot
// retains some. The slot is dropped when the test ends.
func (f runnerFixture) plantSlotRetainingWAL(t *testing.T, name string) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), `SELECT pg_create_logical_replication_slot($1, 'pgoutput')`, name)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := f.pool.Exec(context.WithoutCancel(t.Context()), `SELECT pg_drop_replication_slot($1)`, name)
		assert.NoError(t, err)
	})
	_, err = f.pool.Exec(t.Context(), `SELECT pg_logical_emit_message(false, 'pg-sprite-test', repeat('x', 65536))`)
	require.NoError(t, err)
	require.Positive(t, f.slotRetained(t, name), "the slot retains WAL")
}

// slotRetained is the WAL the named slot retains, from its restart
// position to the server's write position.
func (f runnerFixture) slotRetained(t *testing.T, slot string) int64 {
	t.Helper()
	var retained int64
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)::bigint
		FROM pg_replication_slots
		WHERE slot_name = $1`, slot).Scan(&retained))
	return retained
}
