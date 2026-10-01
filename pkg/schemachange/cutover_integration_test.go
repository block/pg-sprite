package schemachange_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/schemachange"
)

// relationOID resolves one relation of the fixture schema by name.
func (f shadowFixture) relationOID(t *testing.T, name string) uint32 {
	t.Helper()
	var oid uint32
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT c.oid FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2`, f.schema, name).Scan(&oid))
	return oid
}

// indexNames lists a table's indexes by name, sorted.
func (f shadowFixture) indexNames(t *testing.T, table string) []string {
	t.Helper()
	var names []string
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT ARRAY(SELECT ic.relname FROM pg_index i JOIN pg_class ic ON ic.oid = i.indexrelid
		             WHERE i.indrelid = to_regclass($1) ORDER BY ic.relname)`,
		pgx.Identifier{f.schema, table}.Sanitize()).Scan(&names))
	return names
}

// statisticsNames lists a table's extended-statistics objects by name, sorted.
func (f shadowFixture) statisticsNames(t *testing.T, table string) []string {
	t.Helper()
	var names []string
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT ARRAY(SELECT stxname FROM pg_statistic_ext WHERE stxrelid = to_regclass($1) ORDER BY stxname)`,
		pgx.Identifier{f.schema, table}.Sanitize()).Scan(&names))
	return names
}

// serialSequence is the schema-qualified sequence a column owns, or "" when
// it owns none, as pg_get_serial_sequence reports it (unquoted: the fixture
// schema and the sequence names need no quoting).
func (f shadowFixture) serialSequence(t *testing.T, table, column string) string {
	t.Helper()
	var sequence *string
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT pg_get_serial_sequence($1, $2)`,
		pgx.Identifier{f.schema, table}.Sanitize(), column).Scan(&sequence))
	if sequence == nil {
		return ""
	}
	return *sequence
}

// sequenceDeclaration reads a sequence's declared start and increment.
func (f shadowFixture) sequenceDeclaration(t *testing.T, sequence string) (start, increment int64) {
	t.Helper()
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT seqstart, seqincrement FROM pg_sequence WHERE seqrelid = to_regclass($1)`,
		pgx.Identifier{f.schema, sequence}.Sanitize()).Scan(&start, &increment))
	return start, increment
}

// rowCount counts a table's rows.
func (f shadowFixture) rowCount(t *testing.T, table string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+pgx.Identifier{f.schema, table}.Sanitize()).Scan(&n))
	return n
}

// cutover gates and swaps a staged table with no drain.
func (f shadowFixture) cutover(t *testing.T, s staged, opts schemachange.CutoverOptions) (schemachange.SwappedTable, error) {
	t.Helper()
	ready, err := f.gate(t, s)
	require.NoError(t, err)
	return schemachange.Cutover(t.Context(), f.pool, s.lock, ready, nil, opts)
}

// holdAccessShare opens a transaction that reads the table and keeps it
// open, so it holds ACCESS SHARE on the table until released: the lightest
// lock that still excludes ACCESS EXCLUSIVE. The returned func releases it.
func (f shadowFixture) holdAccessShare(t *testing.T, table string) func() {
	t.Helper()
	holder, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	_, err = holder.Exec(t.Context(), `SELECT 1 FROM `+pgx.Identifier{f.schema, table}.Sanitize()+` LIMIT 1`)
	require.NoError(t, err)
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		assert.NoError(t, holder.Rollback(context.WithoutCancel(t.Context())))
	}
	t.Cleanup(release)
	return release
}

// Dropping a column through copy-and-swap: after Cutover the shadow bears
// the source's name and OID-for-OID is the shadow the build created; its
// primary key, qty index, and statistics object bear the source's names
// (the index on the dropped column had no partner and stays with the old
// table under a derived name); the serial sequence is owned by the live
// table and keeps counting from where the source left it; and the retained
// source sits under its _old name until DropOldTable removes it along with
// its renamed indexes, leaving the live table and its sequence intact (D5,
// D8, D9).
func TestCutoverSwapsTheShadowInAndDropOldTableRemovesTheSource(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)
	old := schemachange.OldName(f.schema, "orders")
	oldDependent := func(name string) string { return schemachange.OldDependentName(f.schema, "orders", name) }

	swapped, err := f.cutover(t, s, schemachange.CutoverOptions{})
	require.NoError(t, err)

	assert.Equal(t, "orders", swapped.Table())
	assert.Equal(t, old, swapped.OldTable())
	assert.Equal(t, 1, swapped.Attempts())
	assert.Equal(t, s.built.ShadowOID(), f.relationOID(t, "orders"), "the live orders is the shadow the build created")
	assert.Equal(t, s.built.SourceOID(), f.relationOID(t, old), "the source is retained under its _old name")
	assert.False(t, f.relationExists(t, s.built.ShadowTable()), "nothing bears the shadow name any more")
	assert.Equal(t, int64(2500), f.rowCount(t, "orders"))

	assert.Equal(t, []string{"orders_pkey", "orders_qty_idx"}, f.indexNames(t, "orders"))
	assert.ElementsMatch(t, []string{oldDependent("orders_note_idx"), oldDependent("orders_pkey"), oldDependent("orders_qty_idx")}, f.indexNames(t, old),
		"every source index, paired or not, moved to a derived name with the old table")
	assert.Equal(t, []string{"orders_qty_sku_stat"}, f.statisticsNames(t, "orders"))
	assert.Equal(t, []string{oldDependent("orders_qty_sku_stat")}, f.statisticsNames(t, old))
	var constraintIndex string
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT conindid::regclass::text FROM pg_constraint WHERE conrelid = to_regclass($1) AND conname = 'orders_pkey'`,
		pgx.Identifier{f.schema, "orders"}.Sanitize()).Scan(&constraintIndex))
	assert.Equal(t, f.schema+".orders_pkey", constraintIndex, "the primary key constraint moved with its index")

	assert.Equal(t, f.schema+".orders_id_seq", f.serialSequence(t, "orders", "id"), "the serial sequence is owned by the live column")
	assert.Equal(t, "", f.serialSequence(t, old, "id"), "and no longer by the old one")
	var nextID int64
	require.NoError(t, f.pool.QueryRow(t.Context(), `INSERT INTO `+pgx.Identifier{f.schema, "orders"}.Sanitize()+` (qty, sku) VALUES (1, 'new') RETURNING id`).Scan(&nextID))
	assert.Equal(t, int64(2501), nextID, "the shared counter continues from the source's last value")

	require.NoError(t, schemachange.DropOldTable(t.Context(), f.pool, s.lock, swapped, schemachange.Options{}))

	assert.False(t, f.relationExists(t, old))
	assert.False(t, f.relationExists(t, oldDependent("orders_note_idx")), "the old table's indexes go with it")
	assert.True(t, f.relationExists(t, "orders_id_seq"), "the re-owned sequence survives the drop")
	require.NoError(t, f.pool.QueryRow(t.Context(), `INSERT INTO `+pgx.Identifier{f.schema, "orders"}.Sanitize()+` (qty, sku) VALUES (2, 'after') RETURNING id`).Scan(&nextID))
	assert.Equal(t, int64(2502), nextID)
	assert.Equal(t, int64(2502), f.rowCount(t, "orders"))
}

// Identity columns cross the swap as identity columns, not as the plain
// nextval defaults the shadow carried: the live table's id is GENERATED
// ALWAYS again with the source's INCREMENT BY 2 and START WITH 10 declared
// on a sequence under the source sequence's name, and the next id continues
// the source's series with no gap. A BY DEFAULT identity whose sequence the
// source never advanced (every row supplied seqno) stays unadvanced, so its
// first generated value is its start, not the one after. The old table's
// identity sequences, renamed out of the way, are dropped with it (D5).
func TestCutoverHandsOffIdentityColumnsWithoutAGap(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.tickets (
			id    bigint GENERATED ALWAYS AS IDENTITY (INCREMENT BY 2 START WITH 10) PRIMARY KEY,
			seqno bigint GENERATED BY DEFAULT AS IDENTITY,
			qty   integer NOT NULL
		)`)
	f.exec(t, `
		INSERT INTO %s.tickets (seqno, qty)
		SELECT g, g
		FROM generate_series(1, 2500) g`)
	s := f.stage(t, "tickets", `ALTER TABLE %s.tickets ALTER COLUMN qty TYPE bigint`)
	old := schemachange.OldName(f.schema, "tickets")

	swapped, err := f.cutover(t, s, schemachange.CutoverOptions{})
	require.NoError(t, err)

	assert.Equal(t, s.built.IdentityColumns(), swapped.IdentityColumns())
	def, identity := f.columnDefault(t, "tickets", "id")
	assert.Equal(t, "", def, "the handoff default is gone")
	assert.Equal(t, "a", identity, "id is GENERATED ALWAYS again")
	_, identity = f.columnDefault(t, "tickets", "seqno")
	assert.Equal(t, "d", identity, "seqno is GENERATED BY DEFAULT again")
	assert.Equal(t, f.schema+".tickets_id_seq", f.serialSequence(t, "tickets", "id"), "the recreated sequence bears the source sequence's name")
	assert.Equal(t, f.schema+".tickets_seqno_seq", f.serialSequence(t, "tickets", "seqno"))
	start, increment := f.sequenceDeclaration(t, "tickets_id_seq")
	assert.Equal(t, int64(10), start, "START WITH is the source's declaration, not the counter's position")
	assert.Equal(t, int64(2), increment)
	assert.True(t, f.relationExists(t, schemachange.OldDependentName(f.schema, "tickets", "tickets_id_seq")), "the source's identity sequence is retained with the old table")

	var nextID, nextSeqno int64
	require.NoError(t, f.pool.QueryRow(t.Context(), `INSERT INTO `+pgx.Identifier{f.schema, "tickets"}.Sanitize()+` (qty) VALUES (1) RETURNING id, seqno`).Scan(&nextID, &nextSeqno))
	// 2500 rows at increment 2 from 10 end at 5008; the next is 5010.
	assert.Equal(t, int64(5010), nextID, "the id series continues without a gap")
	assert.Equal(t, int64(1), nextSeqno, "a never-advanced sequence issues its start value first")

	require.NoError(t, schemachange.DropOldTable(t.Context(), f.pool, s.lock, swapped, schemachange.Options{}))
	assert.False(t, f.relationExists(t, old))
	assert.False(t, f.relationExists(t, schemachange.OldDependentName(f.schema, "tickets", "tickets_id_seq")), "the old identity sequence goes with the old table")
	assert.True(t, f.relationExists(t, "tickets_id_seq"))
	require.NoError(t, f.pool.QueryRow(t.Context(), `INSERT INTO `+pgx.Identifier{f.schema, "tickets"}.Sanitize()+` (qty) VALUES (2) RETURNING id, seqno`).Scan(&nextID, &nextSeqno))
	assert.Equal(t, int64(5012), nextID)
	assert.Equal(t, int64(2), nextSeqno)
}

// The ACCESS EXCLUSIVE acquisition is bounded and retried, never queued
// (LK-2): a reader holding ACCESS SHARE makes the first attempt time out
// under lock_timeout; the swap backs off through the injected sleep, and
// the attempt after the reader lets go succeeds. The reader is released
// from inside the sleep, so the retry provably ran after the backoff.
func TestCutoverRetriesTheLockBehindAReaderAndSucceedsOnceItReleases(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)
	release := f.holdAccessShare(t, "orders")

	var slept []time.Duration
	opts := schemachange.CutoverOptions{
		Options:      schemachange.Options{LockTimeout: 200 * time.Millisecond},
		LockAttempts: 3,
		LockBackoff:  time.Second,
		Sleep: func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			release()
			return nil
		},
	}
	swapped, err := f.cutover(t, s, opts)
	require.NoError(t, err)

	assert.Equal(t, 2, swapped.Attempts())
	assert.Equal(t, []time.Duration{time.Second}, slept, "one backoff of one step before the attempt that succeeded")
	assert.Equal(t, s.built.ShadowOID(), f.relationOID(t, "orders"))
}

// A reader that never lets go exhausts the bounded attempts: the swap
// returns ErrLockRetriesExhausted wrapping the server's lock_timeout answer
// after exactly the configured attempts, each later backoff one step
// longer, and nothing is swapped — the source is still live and the shadow
// still in place for a later retry (LK-2).
func TestCutoverStopsAfterTheBoundedLockAttempts(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)
	f.holdAccessShare(t, "orders")

	var slept []time.Duration
	opts := schemachange.CutoverOptions{
		Options:      schemachange.Options{LockTimeout: 100 * time.Millisecond},
		LockAttempts: 3,
		LockBackoff:  time.Millisecond,
		Sleep: func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			return nil
		},
	}
	_, err := f.cutover(t, s, opts)

	require.ErrorIs(t, err, schemachange.ErrLockRetriesExhausted)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "55P03", pgErr.Code, "the last answer was lock_not_available")
	assert.Equal(t, []time.Duration{time.Millisecond, 2 * time.Millisecond}, slept, "two backoffs between three attempts")
	assert.Equal(t, s.built.SourceOID(), f.relationOID(t, "orders"), "the source is still live")
	assert.True(t, f.relationExists(t, s.built.ShadowTable()), "the shadow is kept for a later attempt")
	assert.False(t, f.relationExists(t, schemachange.OldName(f.schema, "orders")))
}

// The drain runs inside the swap transaction after ACCESS EXCLUSIVE on the
// source is granted, so a row it writes into the shadow is in the live
// table the moment the swap commits and no writer could have raced it; a
// drain that fails aborts the whole transaction, and nothing is swapped.
func TestCutoverRunsTheDrainUnderTheLockAndAbortsOnItsError(t *testing.T) {
	t.Run("drained row lands in the live table", func(t *testing.T) {
		f := newShadowFixture(t)
		f.createOrders(t)
		s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)
		ready, err := f.gate(t, s)
		require.NoError(t, err)

		var lockedWhileDraining bool
		drain := func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM pg_locks
					WHERE pid = pg_backend_pid() AND relation = $1 AND mode = 'AccessExclusiveLock' AND granted)`,
				s.built.SourceOID()).Scan(&lockedWhileDraining); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO `+f.shadowName(s.built)+` (id, qty, sku) VALUES (9001, 1, 'drained')`)
			return err
		}
		_, err = schemachange.Cutover(t.Context(), f.pool, s.lock, ready, drain, schemachange.CutoverOptions{})
		require.NoError(t, err)

		assert.True(t, lockedWhileDraining, "the drain saw its own ACCESS EXCLUSIVE lock on the source")
		var sku string
		require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT sku FROM `+pgx.Identifier{f.schema, "orders"}.Sanitize()+` WHERE id = 9001`).Scan(&sku))
		assert.Equal(t, "drained", sku)
		assert.Equal(t, int64(2501), f.rowCount(t, "orders"))
	})
	t.Run("drain error aborts the swap", func(t *testing.T) {
		f := newShadowFixture(t)
		f.createOrders(t)
		s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)
		ready, err := f.gate(t, s)
		require.NoError(t, err)

		drainFailed := errors.New("applier lost its slot")
		drain := func(context.Context, pgx.Tx) error { return drainFailed }
		_, err = schemachange.Cutover(t.Context(), f.pool, s.lock, ready, drain, schemachange.CutoverOptions{})

		require.ErrorIs(t, err, drainFailed)
		assert.NotErrorIs(t, err, schemachange.ErrLockRetriesExhausted, "a drain failure is not retried")
		assert.Equal(t, s.built.SourceOID(), f.relationOID(t, "orders"), "the source is still live")
		assert.True(t, f.relationExists(t, s.built.ShadowTable()))
	})
}

// A connection lost mid-swap says nothing about the outcome, so the swap
// asks the catalog from a fresh connection before reporting (LK-4). Here
// the swap's own backend is terminated after the lock and the drain, so
// the transaction rolled back: the catalog shows the source still live, and
// the swap reports the lost connection as a rollback — not as an invariant
// violation and not as something to retry blindly — leaving the source and
// the shadow where they were.
func TestCutoverResolvesALostConnectionByInspectingTheCatalog(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)
	ready, err := f.gate(t, s)
	require.NoError(t, err)

	drain := func(ctx context.Context, tx pgx.Tx) error {
		var pid uint32
		if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			return err
		}
		f.terminateBackend(t, pid)
		return nil
	}
	_, err = schemachange.Cutover(t.Context(), f.pool, s.lock, ready, drain, schemachange.CutoverOptions{})

	require.Error(t, err)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "57P01", pgErr.Code, "the server's admin_shutdown is what the swap saw")
	assert.NotErrorIs(t, err, schemachange.ErrInvariantViolation, "a rolled-back attempt is a plain failure, not an ambiguity")
	assert.NotErrorIs(t, err, schemachange.ErrLockRetriesExhausted)
	assert.Equal(t, s.built.SourceOID(), f.relationOID(t, "orders"), "the source is still live")
	assert.True(t, f.relationExists(t, s.built.ShadowTable()))
	assert.Equal(t, []string{"orders_note_idx", "orders_pkey", "orders_qty_idx"}, f.indexNames(t, "orders"), "no rename survived the rollback")
}

// Cutover takes only a proof the gate minted and only under the per-table
// lock: a zero proof is refused as unverified (CO-1) and a missing lock as
// unproven (LK-1), both before any connection opens.
func TestCutoverRefusesAZeroProofAndAMissingLock(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)
	ready, err := f.gate(t, s)
	require.NoError(t, err)

	_, err = schemachange.Cutover(t.Context(), f.pool, s.lock, schemachange.CutoverReady{}, nil, schemachange.CutoverOptions{})
	assert.Equal(t, schemachange.CauseCutoverUnverified, schemachange.RefusalCauseOf(err), "zero proof")

	_, err = schemachange.Cutover(t.Context(), f.pool, nil, ready, nil, schemachange.CutoverOptions{})
	assert.Equal(t, schemachange.CauseLockUnproven, schemachange.RefusalCauseOf(err), "no lock")

	assert.Equal(t, s.built.SourceOID(), f.relationOID(t, "orders"), "nothing was swapped")
}

// DropOldTable drops only the relation the swap retained: a different
// relation that has since taken the _old name is refused on its OID and
// left standing (ST-6), a zero proof is refused before connecting, and the
// genuine old table is still there to be dropped once the impostor is out
// of the way.
func TestDropOldTableRefusesARelationThatIsNotTheRetainedSource(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)
	swapped, err := f.cutover(t, s, schemachange.CutoverOptions{})
	require.NoError(t, err)
	old := schemachange.OldName(f.schema, "orders")

	err = schemachange.DropOldTable(t.Context(), f.pool, s.lock, schemachange.SwappedTable{}, schemachange.Options{})
	assert.Equal(t, schemachange.CauseProofEmpty, schemachange.RefusalCauseOf(err), "zero proof")

	f.exec(t, fmt.Sprintf(`ALTER TABLE %%s.%s RENAME TO orders_retained`, old))
	f.exec(t, fmt.Sprintf(`CREATE TABLE %%s.%s (impostor integer)`, old))
	err = schemachange.DropOldTable(t.Context(), f.pool, s.lock, swapped, schemachange.Options{})
	assert.Equal(t, schemachange.CauseRelationReplaced, schemachange.RefusalCauseOf(err))
	assert.True(t, f.relationExists(t, old), "the impostor is not dropped")
	assert.True(t, f.relationExists(t, "orders_retained"), "nor is the retained source")

	f.exec(t, fmt.Sprintf(`DROP TABLE %%s.%s`, old))
	f.exec(t, fmt.Sprintf(`ALTER TABLE %%s.orders_retained RENAME TO %s`, old))
	require.NoError(t, schemachange.DropOldTable(t.Context(), f.pool, s.lock, swapped, schemachange.Options{}))
	assert.False(t, f.relationExists(t, old))
	assert.Equal(t, s.built.ShadowOID(), f.relationOID(t, "orders"), "the live table is untouched")
}
