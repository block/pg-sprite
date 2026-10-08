package decode_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/preflight"
)

// The stream is refused before any connection is made when the target is
// not a decoding target or the start position is missing.
func TestOpenStreamRefusesTheZeroTargetAndAMissingPosition(t *testing.T) {
	stream, err := decode.OpenStream(t.Context(), dbconn.Config{}, preflight.CopySwapTarget{}, 1)
	require.ErrorIs(t, err, decode.ErrInvariantViolation)
	assert.Nil(t, stream)

	f := newSlotFixture(t)
	stream, err = decode.OpenStream(t.Context(), f.cfg, f.target, 0)
	require.ErrorIs(t, err, decode.ErrInvariantViolation)
	assert.Nil(t, stream)
}

// An INSERT arrives as a complete image — every column present, a text
// value as a string — keyed by the primary key, at a WAL position the slot
// had not yet reached when it was created, and the transaction's commit
// moves the delivered position past it.
func TestStreamDecodesAnInsert(t *testing.T) {
	f := newSlotFixture(t)
	slot := f.createSlot(t)
	stream := f.openStream(t, slot.ConsistentPoint())

	f.exec(t, `INSERT INTO %s.ledger (id, note) VALUES (101, 'after the slot')`)

	ev := nextChange(t, stream)
	assert.Equal(t, decode.Insert, ev.Kind)
	assert.EqualValues(t, 101, ev.Key)
	assert.Nil(t, ev.OldKey)
	assert.Equal(t, []decode.Column{column("id", "101"), column("note", "after the slot")}, ev.Columns)
	assert.GreaterOrEqual(t, ev.LSN, slot.ConsistentPoint())

	awaitDelivered(t, stream, ev.LSN+1)
	assert.Greater(t, stream.Delivered(), ev.LSN, "the commit moves the delivered position past the change")
}

// An INSERT of a NULL carries the column present with no value: NULL is a
// value the applier writes, unlike an omitted column.
func TestStreamDecodesANullAsAPresentColumn(t *testing.T) {
	f := newSlotFixture(t)
	slot := f.createSlot(t)
	stream := f.openStream(t, slot.ConsistentPoint())

	f.exec(t, `INSERT INTO %s.ledger (id, note) VALUES (101, NULL)`)

	ev := nextChange(t, stream)
	assert.Equal(t, decode.Insert, ev.Kind)
	assert.Equal(t, []decode.Column{column("id", "101"), {Name: "note", Value: nil, Present: true}}, ev.Columns)
}

// An UPDATE that leaves a column stored out of line untouched arrives with
// that column absent — pgoutput sends the unchanged-TOAST marker, not the
// value — while the columns the statement did change are present (CO-8).
// The storage is forced out of line so the marker is emitted regardless of
// how well the value compresses.
func TestStreamDecodesAnUpdateWithAnUnchangedToastValueAbsent(t *testing.T) {
	f := newSlotFixture(t)
	f.exec(t, `ALTER TABLE %s.ledger ADD COLUMN flag integer`)
	f.exec(t, `ALTER TABLE %s.ledger ALTER COLUMN note SET STORAGE EXTERNAL`)
	f.exec(t, `UPDATE %s.ledger SET note = repeat('x', 4000) WHERE id = 7`)
	slot := f.createSlot(t)
	stream := f.openStream(t, slot.ConsistentPoint())

	f.exec(t, `UPDATE %s.ledger SET flag = 1 WHERE id = 7`)

	ev := nextChange(t, stream)
	assert.Equal(t, decode.Update, ev.Kind)
	assert.EqualValues(t, 7, ev.Key)
	assert.Nil(t, ev.OldKey, "the key did not move")
	assert.Equal(t, []decode.Column{column("id", "7"), absentColumn("note"), column("flag", "1")}, ev.Columns)
}

// An UPDATE that changes the out-of-line column carries its new value, so
// the marker is only ever sent for a value the statement left alone.
func TestStreamDecodesAnUpdateOfAToastValueAsPresent(t *testing.T) {
	f := newSlotFixture(t)
	f.exec(t, `ALTER TABLE %s.ledger ALTER COLUMN note SET STORAGE EXTERNAL`)
	f.exec(t, `UPDATE %s.ledger SET note = repeat('x', 4000) WHERE id = 7`)
	slot := f.createSlot(t)
	stream := f.openStream(t, slot.ConsistentPoint())

	f.exec(t, `UPDATE %s.ledger SET note = repeat('y', 4000) WHERE id = 7`)

	ev := nextChange(t, stream)
	assert.Equal(t, decode.Update, ev.Kind)
	require.Len(t, ev.Columns, 2)
	assert.True(t, ev.Columns[1].Present)
	assert.Equal(t, "y", ev.Columns[1].Value.(string)[:1])
	assert.Len(t, ev.Columns[1].Value, 4000)
}

// An UPDATE that moves the primary key carries the key the row had as
// OldKey: under DEFAULT replica identity pgoutput sends a key-only old
// tuple exactly when a key column changed (CO-4 — a move is a deletion and
// an image).
func TestStreamDecodesAKeyMoveWithOldKey(t *testing.T) {
	f := newSlotFixture(t)
	slot := f.createSlot(t)
	stream := f.openStream(t, slot.ConsistentPoint())

	f.exec(t, `UPDATE %s.ledger SET id = 5000 WHERE id = 5`)

	ev := nextChange(t, stream)
	assert.Equal(t, decode.Update, ev.Kind)
	assert.EqualValues(t, 5000, ev.Key)
	require.NotNil(t, ev.OldKey)
	assert.EqualValues(t, 5, *ev.OldKey)
	assert.Equal(t, []decode.Column{column("id", "5000"), column("note", "row 5")}, ev.Columns)
}

// Under REPLICA IDENTITY FULL pgoutput sends the whole old row with every
// UPDATE; OldKey is still set only when the key moved, so an applier never
// mistakes an in-place update for a move.
func TestStreamUnderReplicaIdentityFullSetsOldKeyOnlyOnAMove(t *testing.T) {
	f := newSlotFixture(t)
	f.exec(t, `ALTER TABLE %s.ledger REPLICA IDENTITY FULL`)
	slot := f.createSlot(t)
	stream := f.openStream(t, slot.ConsistentPoint())

	f.exec(t, `UPDATE %s.ledger SET note = 'edited' WHERE id = 5`)
	f.exec(t, `UPDATE %s.ledger SET id = 5000 WHERE id = 5`)
	f.exec(t, `DELETE FROM %s.ledger WHERE id = 6`)

	changes := nextChanges(t, stream, 3)
	assert.Equal(t, decode.Update, changes[0].Kind)
	assert.EqualValues(t, 5, changes[0].Key)
	assert.Nil(t, changes[0].OldKey, "an in-place update under FULL is not a move")
	assert.Equal(t, decode.Update, changes[1].Kind)
	assert.EqualValues(t, 5000, changes[1].Key)
	require.NotNil(t, changes[1].OldKey)
	assert.EqualValues(t, 5, *changes[1].OldKey)
	assert.Equal(t, decode.Delete, changes[2].Kind)
	assert.EqualValues(t, 6, changes[2].Key)
}

// A DELETE arrives keyed by the deleted row's primary key with no columns.
func TestStreamDecodesADelete(t *testing.T) {
	f := newSlotFixture(t)
	slot := f.createSlot(t)
	stream := f.openStream(t, slot.ConsistentPoint())

	f.exec(t, `DELETE FROM %s.ledger WHERE id = 42`)

	ev := nextChange(t, stream)
	assert.Equal(t, decode.Delete, ev.Kind)
	assert.EqualValues(t, 42, ev.Key)
	assert.Nil(t, ev.OldKey)
	assert.Nil(t, ev.Columns)
}

// The changes of one transaction arrive in statement order, each at its
// own WAL position, and the delivered position stays where it was until
// the transaction commits: nothing inside the transaction is confirmable
// before its last change has been yielded.
func TestStreamDeliversATransactionsChangesBeforeMovingThePosition(t *testing.T) {
	f := newSlotFixture(t)
	slot := f.createSlot(t)
	stream := f.openStream(t, slot.ConsistentPoint())
	before := stream.Delivered()

	f.exec(t, `
		BEGIN;
		INSERT INTO %[1]s.ledger (id, note) VALUES (101, 'first');
		UPDATE %[1]s.ledger SET note = 'second' WHERE id = 101;
		DELETE FROM %[1]s.ledger WHERE id = 1;
		COMMIT`)

	first, err := stream.Next(t.Context(), streamWait)
	require.NoError(t, err)
	for first.Change == nil {
		first, err = stream.Next(t.Context(), streamWait)
		require.NoError(t, err)
	}
	assert.Equal(t, decode.Insert, first.Change.Kind)
	assert.Equal(t, before, first.Delivered, "the position does not move inside the transaction")

	second, err := stream.Next(t.Context(), streamWait)
	require.NoError(t, err)
	require.NotNil(t, second.Change)
	assert.Equal(t, decode.Update, second.Change.Kind)
	assert.Equal(t, before, second.Delivered)
	assert.Greater(t, second.Change.LSN, first.Change.LSN)

	third, err := stream.Next(t.Context(), streamWait)
	require.NoError(t, err)
	require.NotNil(t, third.Change)
	assert.Equal(t, decode.Delete, third.Change.Kind)
	assert.EqualValues(t, 1, third.Change.Key)
	assert.Equal(t, before, third.Delivered)

	commit, err := stream.Next(t.Context(), streamWait)
	require.NoError(t, err)
	assert.Nil(t, commit.Change, "the commit is a progress-only delivery")
	assert.Greater(t, commit.Delivered, third.Change.LSN)
}

// A statement on the published table inside a transaction that rolls back
// is never decoded; the next committed change is what arrives.
func TestStreamSkipsARolledBackTransaction(t *testing.T) {
	f := newSlotFixture(t)
	slot := f.createSlot(t)
	stream := f.openStream(t, slot.ConsistentPoint())

	f.exec(t, `
		BEGIN;
		INSERT INTO %[1]s.ledger (id, note) VALUES (101, 'rolled back');
		ROLLBACK`)
	f.exec(t, `INSERT INTO %s.ledger (id, note) VALUES (102, 'committed')`)

	ev := nextChange(t, stream)
	assert.EqualValues(t, 102, ev.Key)
}

// A column added to the source after the stream described it is a shape
// change: the next change arrives with a relation the stream does not
// recognise and the stream stops with ErrSourceShapeChanged rather than
// yield columns the shadow has no home for. The error persists.
func TestStreamStopsWhenTheSourceGainsAColumn(t *testing.T) {
	f := newSlotFixture(t)
	slot := f.createSlot(t)
	stream := f.openStream(t, slot.ConsistentPoint())

	f.exec(t, `INSERT INTO %s.ledger (id, note) VALUES (101, 'before the change')`)
	ev := nextChange(t, stream)
	assert.EqualValues(t, 101, ev.Key)

	f.exec(t, `ALTER TABLE %s.ledger ADD COLUMN flag integer`)
	f.exec(t, `INSERT INTO %s.ledger (id, note, flag) VALUES (102, 'after the change', 1)`)

	err := nextError(t, stream)
	require.ErrorIs(t, err, decode.ErrSourceShapeChanged)
	_, err = stream.Next(t.Context(), streamWait)
	assert.ErrorIs(t, err, decode.ErrSourceShapeChanged, "a stopped stream stays stopped")
}

// A TRUNCATE is published as a change the route does not replay by rows,
// so the stream stops with ErrUnsupportedChange.
func TestStreamStopsOnATruncate(t *testing.T) {
	f := newSlotFixture(t)
	slot := f.createSlot(t)
	stream := f.openStream(t, slot.ConsistentPoint())

	f.exec(t, `TRUNCATE %s.ledger`)

	err := nextError(t, stream)
	require.ErrorIs(t, err, decode.ErrUnsupportedChange)
}
