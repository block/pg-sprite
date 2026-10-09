package decode_test

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

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/preflight"
)

const (
	sqlstateInvalidParameterValue = "22023"
	sqlstateUndefinedObject       = "42704"
	sqlstateInsufficientPrivilege = "42501"
)

// requireSnapshotGone asserts that importing the snapshot failed because
// the server no longer has its export file: an invalid parameter value
// before PostgreSQL 17, an undefined object from 17 on. The assertion is
// exact for the server under test, so a refusal for any other reason — the
// exporting transaction still winding down, say — fails it rather than
// passing as "gone".
func requireSnapshotGone(t *testing.T, f slotFixture, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	const reportsAMissingSnapshotAsUndefinedSince = 170000
	want := sqlstateInvalidParameterValue
	if f.serverVersion(t) >= reportsAMissingSnapshotAsUndefinedSince {
		want = sqlstateUndefinedObject
	}
	assert.Equal(t, want, pgErr.Code)
}

// Creating the slot exports a snapshot another session can read from: rows
// written after the slot exists are invisible to it, and the consistent
// point — where decoding on the slot begins — is a position the server had
// already reached. The snapshot ends when the creating connection's
// walsender exits, which is after Close returns; the slot outlives both.
func TestCreateSlotExportsASnapshotAtTheConsistentPoint(t *testing.T) {
	f := newSlotFixture(t)
	before := f.currentWALLSN(t)

	slot := f.createSlot(t)
	assert.Equal(t, f.target.DecodingName(), slot.Name())
	assert.NotEmpty(t, slot.SnapshotName())
	assert.GreaterOrEqual(t, slot.ConsistentPoint(), before, "the consistent point is reached, not predicted")
	assert.LessOrEqual(t, slot.ConsistentPoint(), f.currentWALLSN(t))

	_, err := f.pool.Exec(t.Context(), fmt.Sprintf(`INSERT INTO %s.ledger (id, note) VALUES (101, 'after the slot')`, f.schema))
	require.NoError(t, err)
	tx, err := f.importSnapshot(t, slot.SnapshotName())
	require.NoError(t, err)
	assert.EqualValues(t, 100, f.ledgerRows(t, tx), "the exported snapshot predates the insert")
	require.NoError(t, tx.Rollback(t.Context()))

	status, found, err := decode.InspectSlot(t.Context(), f.pool, slot.Name())
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, status.Logical)
	assert.Equal(t, f.target.Database(), status.Database)
	assert.Equal(t, slot.ConsistentPoint(), status.ConfirmedFlushLSN, "a new slot has confirmed exactly its consistent point")
	assert.Equal(t, decode.WALStatusReserved, status.WALStatus)
	assert.Equal(t, []string{f.schema + ".ledger"}, f.publishedTables(t, slot.Name()))

	walsender := f.walsenderPID(t)
	require.NoError(t, slot.Close(t.Context()))
	f.waitForBackendExit(t, walsender)
	_, err = f.importSnapshot(t, slot.SnapshotName())
	requireSnapshotGone(t, f, err)
	_, found, err = decode.InspectSlot(t.Context(), f.pool, slot.Name())
	require.NoError(t, err)
	assert.True(t, found, "the slot outlives the connection that created it")
}

// A logical slot of the derived name already in the target's database is the
// route's own earlier slot: creating again reports it as such, reuses the
// publication rather than failing on it, and leaves both exactly as found.
func TestCreateSlotReportsTheRoutesOwnEarlierSlot(t *testing.T) {
	f := newSlotFixture(t)
	first := f.createSlot(t)
	require.NoError(t, first.Close(t.Context()))

	second, err := decode.CreateSlot(t.Context(), f.cfg, f.pool, f.target)
	var exists *decode.SlotExistsError
	require.ErrorAs(t, err, &exists)
	assert.Equal(t, first.Name(), exists.Name)
	assert.Nil(t, second)
	assert.Equal(t, []string{f.schema + ".ledger"}, f.publishedTables(t, first.Name()))
}

// The publication is created before the slot, so a role that may create a
// slot but not a publication is refused with nothing to reap. The target's
// owner role here has REPLICATION but not CREATE on the database.
func TestCreateSlotRefusesWithoutCreateOnTheDatabaseBeforeAnySlotExists(t *testing.T) {
	f := newSlotFixture(t)
	const password = "owner-secret"
	owner := testutil.NewRole(t, f.pool, "LOGIN REPLICATION PASSWORD '"+password+"'")
	for _, sql := range []string{
		fmt.Sprintf(`GRANT USAGE, CREATE ON SCHEMA %s TO %s`, f.schema, owner),
		fmt.Sprintf(`ALTER TABLE %s.ledger OWNER TO %s`, f.schema, owner),
	} {
		_, err := f.pool.Exec(t.Context(), sql)
		require.NoError(t, err, sql)
	}
	ownerCfg := dbconn.Config{URL: withCredentials(t, f.databaseURL, owner, password)}
	ownerPool, err := dbconn.NewPool(t.Context(), ownerCfg)
	require.NoError(t, err)
	t.Cleanup(ownerPool.Close)
	target := f.mintTarget(t, ownerPool, true)

	_, err = decode.CreateSlot(t.Context(), ownerCfg, ownerPool, target)
	requireSQLState(t, err, sqlstateInsufficientPrivilege)
	var privilege *decode.PublicationPrivilegeError
	require.ErrorAs(t, err, &privilege)
	assert.Equal(t, target.DecodingName(), privilege.Publication)
	assert.Equal(t, target.Database(), privilege.Database)
	assert.Equal(t, "CREATE", privilege.Privilege)
	_, found, err := decode.InspectSlot(t.Context(), f.pool, target.DecodingName())
	require.NoError(t, err)
	assert.False(t, found, "no slot is created when the publication is refused")
	assert.False(t, f.publicationExists(t, target.DecodingName()))
}

// The slot is named for the target's database, so a replication connection
// or a pool on any other database is refused before anything is created.
func TestCreateSlotRefusesAConnectionOnAnotherDatabase(t *testing.T) {
	f := newSlotFixture(t)
	otherURL := testutil.NewDatabase(t, f.serverURL)
	other, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: otherURL})
	require.NoError(t, err)
	t.Cleanup(other.Close)

	_, err = decode.CreateSlot(t.Context(), dbconn.Config{URL: otherURL}, f.pool, f.target)
	require.ErrorIs(t, err, decode.ErrInvariantViolation, "the replication connection must be on the target's database")
	_, found, err := decode.InspectSlot(t.Context(), f.pool, f.target.DecodingName())
	require.NoError(t, err)
	assert.False(t, found)
	// The publication was created on the right database before the
	// replication connection was proven wrong; it is the route's to reuse.
	assert.True(t, f.publicationExists(t, f.target.DecodingName()))

	_, err = decode.CreateSlot(t.Context(), f.cfg, other, f.target)
	require.ErrorIs(t, err, decode.ErrInvariantViolation, "the pool must be on the target's database")
	var exists bool
	require.NoError(t, other.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_publication WHERE pubname = $1)`, f.target.DecodingName()).Scan(&exists))
	assert.False(t, exists, "nothing is created on the other database")
}

// A database name is not a server: a replication connection to a database
// of the target's name on another cluster is refused before a slot exists
// anywhere, and nothing is created on the other cluster.
func TestCreateSlotRefusesAReplicationConnectionOnAnotherServer(t *testing.T) {
	f := newSlotFixture(t)
	elsewhere := sameNamedDatabaseElsewhere(t, f)

	slot, err := decode.CreateSlot(t.Context(), elsewhere.cfg, f.pool, f.target)
	require.ErrorIs(t, err, decode.ErrInvariantViolation)
	assert.Nil(t, slot)
	_, found, err := decode.InspectSlot(t.Context(), f.pool, f.target.DecodingName())
	require.NoError(t, err)
	assert.False(t, found, "nothing is created on the pool's cluster")
	_, found, err = decode.InspectSlot(t.Context(), elsewhere.pool, f.target.DecodingName())
	require.NoError(t, err)
	assert.False(t, found, "nothing is created on the other cluster")
}

// A create the caller's context cuts off while the server still waits for a
// consistent point leaves no slot behind: the server drops a slot it never
// finished creating once its walsender goes away, and nothing completes the
// create later when the point becomes reachable.
func TestCreateSlotEndedByItsContextLeavesNoSlot(t *testing.T) {
	f := newSlotFixture(t)
	// An open write transaction keeps the server from reaching a consistent
	// point, so the create blocks for as long as it is held.
	held, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := held.Rollback(context.WithoutCancel(t.Context())); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Logf("roll back held transaction: %v", err)
		}
	})
	_, err = held.Exec(t.Context(), fmt.Sprintf(`INSERT INTO %s.ledger (id, note) VALUES (5000, 'in flight')`, f.schema))
	require.NoError(t, err)

	const createBudget = 500 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), createBudget)
	defer cancel()
	slot, err := decode.CreateSlot(ctx, f.cfg, f.pool, f.target)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Nil(t, slot)

	const slotGone = 10 * time.Second
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		_, found, err := decode.InspectSlot(t.Context(), f.pool, f.target.DecodingName())
		assert.NoError(collect, err)
		assert.False(collect, found, "the unfinished slot is dropped with its walsender")
	}, slotGone, 50*time.Millisecond)
	require.NoError(t, held.Rollback(t.Context()))
	_, found, err := decode.InspectSlot(t.Context(), f.pool, f.target.DecodingName())
	require.NoError(t, err)
	assert.False(t, found, "nothing completes the create once the consistent point is reachable")
	assert.True(t, f.publicationExists(t, f.target.DecodingName()), "the publication stays for the next attempt")
}

// A target that was not verified for a run that decodes WAL has no business
// with a slot; neither has the zero target.
func TestCreateSlotRefusesATargetThatDoesNotDecodeWAL(t *testing.T) {
	f := newSlotFixture(t)
	quiesced := f.mintTarget(t, f.pool, false)

	_, err := decode.CreateSlot(t.Context(), f.cfg, f.pool, quiesced)
	require.ErrorIs(t, err, decode.ErrInvariantViolation)
	_, err = decode.CreateSlot(t.Context(), f.cfg, f.pool, preflight.CopySwapTarget{})
	require.ErrorIs(t, err, decode.ErrInvariantViolation)
	assert.False(t, f.publicationExists(t, f.target.DecodingName()))
}

// The slot's connection closes with the context it is given, so the slot can
// be closed from a cleanup after the test's own context has ended.
func TestSlotCloseIsIdempotentForTheCleanupPath(t *testing.T) {
	f := newSlotFixture(t)
	slot := f.createSlot(t)
	require.NoError(t, slot.Close(t.Context()))
	assert.NoError(t, slot.Close(context.WithoutCancel(t.Context())), "closing a closed connection is a no-op")
}
