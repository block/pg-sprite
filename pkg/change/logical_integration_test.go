package change_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/change"
	"github.com/block/pg-sprite/pkg/decode"
)

// Every committed change reaches the subscription as a key and a whole image
// in the table's column order: an UPDATE that moved the key as a deletion of
// the old key and the image under the new one, a DELETE as its key alone.
// Rows that existed before Start are not replayed.
func TestLogicalSourceDeliversEveryCommittedChange(t *testing.T) {
	f := newSourceFixture(t)
	src, sub := f.started(t)

	f.exec(t, `INSERT INTO %s.ledger (id, note) VALUES (101, 'after start')`)
	f.exec(t, `UPDATE %s.ledger SET note = 'edited' WHERE id = 5`)
	f.exec(t, `DELETE FROM %s.ledger WHERE id = 6`)
	f.exec(t, `UPDATE %s.ledger SET id = 7000 WHERE id = 7`)
	require.NoError(t, src.BlockWait(t.Context()))

	assert.Equal(t, []recorded{
		image(101, "after start"),
		image(5, "edited"),
		deletion(6),
		deletion(7),
		image(7000, "row 7"),
	}, sub.all())
}

// The position moves only when a flush lands every buffered change, and only
// the flushed position is confirmed to the slot: a change that was delivered
// but never flushed is still owed by the slot.
func TestLogicalSourceConfirmsOnlyTheFlushedPosition(t *testing.T) {
	f := newSourceFixture(t)
	src, sub := f.started(t)
	start, err := decode.ParseLSN(src.Position())
	require.NoError(t, err)

	f.exec(t, `INSERT INTO %s.ledger (id, note) VALUES (101, 'buffered')`)
	require.NoError(t, src.BlockWait(t.Context()))
	assert.Equal(t, 1, src.GetDeltaLen())
	assert.False(t, src.AllChangesFlushed())
	assert.Equal(t, start.String(), src.Position(), "nothing flushed, nothing to resume past")
	assert.LessOrEqual(t, f.slotConfirmed(t), start)

	require.NoError(t, src.Flush(t.Context()))
	flushed, err := decode.ParseLSN(src.Position())
	require.NoError(t, err)
	assert.Greater(t, flushed, start)
	assert.True(t, src.AllChangesFlushed())
	assert.Equal(t, []recorded{image(101, "buffered")}, sub.applied)
	f.awaitSlotConfirmed(t, flushed)

	residual, flushes := src.FlushResidual()
	assert.Equal(t, 0, residual)
	assert.Equal(t, 1, flushes)
	assert.Equal(t, 1, src.FeedStats().LastFlushRows)
}

// A source resumed from a flushed position replays what committed after it
// and nothing before it.
func TestLogicalSourceResumesFromAFlushedPosition(t *testing.T) {
	f := newSourceFixture(t)
	first, _ := f.started(t)
	f.exec(t, `INSERT INTO %s.ledger (id, note) VALUES (101, 'flushed')`)
	require.NoError(t, first.BlockWait(t.Context()))
	require.NoError(t, first.Flush(t.Context()))
	pos := first.Position()
	f.exec(t, `INSERT INTO %s.ledger (id, note) VALUES (102, 'never flushed')`)
	require.NoError(t, first.BlockWait(t.Context()))
	first.Close()

	sub := newRecordingSubscription()
	resumed := f.newSource(t, sub, nil)
	require.NoError(t, resumed.AddSubscription(f.table(), nil, nil))
	require.NoError(t, resumed.StartFromPosition(t.Context(), pos))
	assert.Equal(t, pos, resumed.Position())
	require.NoError(t, resumed.BlockWait(t.Context()))

	assert.Equal(t, []recorded{image(102, "never flushed")}, sub.all())
}

// A slot that does not exist cannot be resumed from.
func TestLogicalSourceCannotResumeWithoutTheSlot(t *testing.T) {
	f := newSourceFixture(t)
	src := f.newSource(t, newRecordingSubscription(), nil)
	require.NoError(t, src.AddSubscription(f.table(), nil, nil))

	err := src.StartFromPosition(t.Context(), "0/16B3748")
	require.ErrorIs(t, err, change.ErrPositionNotFound)
}

// Start refuses a slot of the target's name that already exists: a fresh
// start would skip what the existing slot still owes.
func TestLogicalSourceStartRefusesAnExistingSlot(t *testing.T) {
	f := newSourceFixture(t)
	first, _ := f.started(t)
	first.Close()

	src := f.newSource(t, newRecordingSubscription(), nil)
	require.NoError(t, src.AddSubscription(f.table(), nil, nil))
	var exists *decode.SlotExistsError
	require.ErrorAs(t, src.Start(t.Context()), &exists)
}

// An UPDATE that leaves an out-of-line value unchanged has no whole image,
// so the stream stops: OnFatal is told, and BlockWait and Flush return the
// error. The storage is forced out of line so the server omits the value
// regardless of how well it compresses.
func TestLogicalSourceStopsOnAnIncompleteImage(t *testing.T) {
	f := newSourceFixture(t)
	f.exec(t, `ALTER TABLE %s.ledger ALTER COLUMN note SET STORAGE EXTERNAL`)
	f.exec(t, `UPDATE %s.ledger SET note = repeat('x', 4000) WHERE id = 7`)
	f.exec(t, `ALTER TABLE %s.ledger ADD COLUMN flag integer`)
	sub := newRecordingSubscription()
	fatal := make(chan error, 1)
	src := f.newSource(t, sub, func(err error) { fatal <- err })
	tbl := f.table()
	tbl.Columns = append(tbl.Columns, "flag")
	require.NoError(t, src.AddSubscription(tbl, nil, nil))
	require.NoError(t, src.Start(t.Context()))

	f.exec(t, `UPDATE %s.ledger SET flag = 1 WHERE id = 7`)

	require.ErrorIs(t, src.BlockWait(t.Context()), change.ErrIncompleteImage)
	require.ErrorIs(t, <-fatal, change.ErrIncompleteImage)
	require.ErrorIs(t, src.Flush(t.Context()), change.ErrIncompleteImage)
	assert.Empty(t, sub.all())
}

// After Stop the source keeps reading and its position keeps moving, but no
// change reaches the subscription.
func TestLogicalSourceStopEndsDelivery(t *testing.T) {
	f := newSourceFixture(t)
	src, sub := f.started(t)

	src.Stop()
	f.exec(t, `INSERT INTO %s.ledger (id, note) VALUES (101, 'after stop')`)
	require.NoError(t, src.BlockWait(t.Context()))
	require.NoError(t, src.Flush(t.Context()))

	assert.Empty(t, sub.all())
	assert.True(t, src.AllChangesFlushed())
}

// FlushUnderTableLock needs a lock, and flushes under it before and after
// waiting for the stream.
func TestLogicalSourceFlushesUnderTheCallersLock(t *testing.T) {
	f := newSourceFixture(t)
	src, sub := f.started(t)

	require.Error(t, src.FlushUnderTableLock(t.Context(), nil))

	f.exec(t, `INSERT INTO %s.ledger (id, note) VALUES (101, 'under lock')`)
	require.NoError(t, src.FlushUnderTableLock(t.Context(), []change.TableLock{"target lock"}))
	assert.Equal(t, []bool{true, true}, sub.flushedUnderLock())
	assert.Equal(t, []recorded{image(101, "under lock")}, sub.applied)
}

// A verification is handed the next change to the watched row with that
// change already flushed: the subscription has applied it when verify runs.
// The row is written continuously until the verification returns, since the
// watch fires only on a change made after it is armed.
func TestLogicalSourceVerifiesTheNextChangeToARow(t *testing.T) {
	f := newSourceFixture(t)
	src, sub := f.started(t)

	writeCtx, stopWriting := context.WithCancel(t.Context())
	var writer sync.WaitGroup
	writer.Go(func() {
		const writeInterval = 10 * time.Millisecond
		ticker := time.NewTicker(writeInterval)
		defer ticker.Stop()
		for {
			select {
			case <-writeCtx.Done():
				return
			case <-ticker.C:
			}
			if _, err := f.pool.Exec(writeCtx, `UPDATE `+f.schema+`.ledger SET note = 'hot ' || clock_timestamp() WHERE id = 5`); err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("update the watched row: %v", err)
				return
			}
		}
	})
	t.Cleanup(func() { stopWriting(); writer.Wait() })

	watch := change.RowWatch{Schema: f.schema, Table: "ledger", Match: func(key []any) bool { return key[0] == int64(5) }}
	var gotKey, gotImage []any
	err := src.VerifyRowAtNextChange(t.Context(), watch, func(_ context.Context, key, image []any, deleted bool) error {
		assert.False(t, deleted)
		assert.Zero(t, sub.Length(), "the change is flushed before verify runs")
		gotKey, gotImage = key, image
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []any{int64(5)}, gotKey)
	require.Len(t, gotImage, 2)
	assert.Equal(t, "5", gotImage[0])
	assert.Contains(t, sub.applied, recorded{key: gotKey, row: gotImage})
}

// The subscription must be for the target's table and key, there is one per
// source, and only the key column can be declared immutable: the stream does
// not carry a row's prior values.
func TestLogicalSourceRefusesSubscriptionsItCannotFeed(t *testing.T) {
	f := newSourceFixture(t)
	src := f.newSource(t, newRecordingSubscription(), nil)

	other := f.table()
	other.TableName = "other"
	require.Error(t, src.AddSubscription(other, nil, nil))

	wrongKey := f.table()
	wrongKey.KeyColumns = []string{"note"}
	require.Error(t, src.AddSubscription(wrongKey, nil, nil))

	sub := newRecordingSubscription()
	sub.immutable = 1
	immutableNote := f.newSource(t, sub, nil)
	require.Error(t, immutableNote.AddSubscription(f.table(), nil, nil))

	require.NoError(t, src.AddSubscription(f.table(), nil, nil))
	require.Error(t, src.AddSubscription(f.table(), nil, nil))
}

// A subscription that declares the key immutable stops the stream on an
// UPDATE that moves the key.
func TestLogicalSourceStopsOnAMoveOfAnImmutableKey(t *testing.T) {
	f := newSourceFixture(t)
	sub := newRecordingSubscription()
	sub.immutable = 0
	src := f.newSource(t, sub, nil)
	require.NoError(t, src.AddSubscription(f.table(), nil, nil))
	require.NoError(t, src.Start(t.Context()))

	f.exec(t, `UPDATE %s.ledger SET id = 7000 WHERE id = 7`)

	require.ErrorIs(t, src.BlockWait(t.Context()), change.ErrImmutableColumnChanged)
}

// CurrentPosition is the server's WAL position: never behind anything the
// source has read.
func TestLogicalSourceCurrentPositionIsTheServersHead(t *testing.T) {
	f := newSourceFixture(t)
	src, _ := f.started(t)
	f.exec(t, `INSERT INTO %s.ledger (id, note) VALUES (101, 'head')`)
	require.NoError(t, src.BlockWait(t.Context()))

	head, err := src.CurrentPosition(t.Context())
	require.NoError(t, err)
	headLSN, err := decode.ParseLSN(head)
	require.NoError(t, err)
	buffered, err := decode.ParseLSN(src.FeedStats().BufferedPosition)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, headLSN, buffered)
}
