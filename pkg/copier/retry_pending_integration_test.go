package copier_test

import (
	"math"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// seatsRun is a one-worker copy of a 300-row seats table, cut into chunks
// of 100 rows, whose shadow holds a row under each key of held (key to
// slot) from the copier's first clock reading, so the first chunk collides
// on every slot named.
type seatsRun struct {
	c       *copier.Copier
	results chan error
	shadow  schemachange.BuiltShadow
	// name is the shadow's qualified, quoted name.
	name string
	// began is the time of the copier's first clock reading.
	began *time.Time
}

func (f copierFixture) startSeats(t *testing.T, attempts int, backoff time.Duration, held map[int64]int64) seatsRun {
	t.Helper()
	target, lock, shadow := f.prepareSeats(t, 300)
	name := pgx.Identifier{f.schema, shadow.ShadowTable()}.Sanitize()
	began := new(time.Time)
	clock := &hookedClock{hook: func() {
		*began = time.Now()
		for key, slot := range held {
			_, err := f.pool.Exec(t.Context(), "INSERT INTO "+name+" (id, slot) VALUES ($1, $2)", key, slot)
			assert.NoError(t, err)
		}
	}}
	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{
		Workers:           1,
		Chunker:           copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
		CollisionAttempts: attempts,
		CollisionBackoff:  backoff,
		Clock:             clock,
	})
	require.NoError(t, err)
	run := seatsRun{c: c, results: make(chan error, 1), shadow: shadow, name: name, began: began}
	var wg sync.WaitGroup
	wg.Go(func() { run.results <- c.Run(t.Context(), f.pool) })
	t.Cleanup(wg.Wait)
	return run
}

// awaitPending waits until the one collided chunk holds exactly keys keys
// pending.
func (r seatsRun) awaitPending(t *testing.T, keys int) {
	t.Helper()
	const pendingDeadline = 15 * time.Second
	require.Eventually(t, func() bool {
		p := r.c.Position().Pending
		return len(p) == 1 && len(p[0].Keys) == keys
	}, pendingDeadline, 2*time.Millisecond)
}

// The pending keys shrink as the shadow rows holding their slots go: with
// slots 15, 25, and 35 held, the chunk commits around all three; removing
// the holder of slot 25 lands key 25 alone on the next pass, and the chunk
// lands once the other two holders go. Each row counts as inserted once.
func TestCopierPendingKeysShrinkAsTheirHoldersGo(t *testing.T) {
	f := newCopierFixture(t)
	run := f.startSeats(t, 50, 20*time.Millisecond, map[int64]int64{2000: 15, 2001: 25, 2002: 35})
	run.awaitPending(t, 3)
	pos := run.c.Position()
	assert.Equal(t, []copier.PendingKeys{{Chunk: pos.InFlight[0], Keys: []int64{15, 25, 35}}}, pos.Pending)

	f.exec(t, "DELETE FROM "+run.name+" WHERE id = 2001")
	run.awaitPending(t, 2)
	assert.Equal(t, []int64{15, 35}, run.c.Position().Pending[0].Keys)
	assert.Equal(t, copier.KeyLanded, run.c.Position().Classify(25), "key 25 landed once its slot was free")

	f.exec(t, "DELETE FROM "+run.name+" WHERE id IN (2000, 2002)")
	const stopDeadline = 30 * time.Second
	require.NoError(t, awaitRun(t, run.results, stopDeadline, "copy after every holder is removed"))
	f.assertConverged(t, run.shadow)
	assert.Equal(t, int64(300), run.c.Position().RowsInserted)
	assert.Equal(t, copier.NewWatermark(math.MaxInt64), run.c.Position().Watermark)
}

// After the chunk commits around key 15, the passes that follow read key 15
// alone, never the range again: a source row that arrives in the chunk's
// range afterwards (key -5) under a slot another shadow row holds is the
// catch-up's to write, so it cannot make the chunk collide anew, and the
// chunk lands without it once key 15's holder goes.
func TestCopierLaterPassesReadOnlyThePendingKeys(t *testing.T) {
	f := newCopierFixture(t)
	run := f.startSeats(t, 8, 20*time.Millisecond, map[int64]int64{2000: 15})
	run.awaitPending(t, 1)
	f.exec(t, "INSERT INTO %s.seats (id, slot) VALUES (-5, 9000)")
	f.exec(t, "INSERT INTO "+run.name+" (id, slot) VALUES (2001, 9000)")
	f.exec(t, "DELETE FROM "+run.name+" WHERE id = 2000")

	const stopDeadline = 30 * time.Second
	require.NoError(t, awaitRun(t, run.results, stopDeadline, "copy after key 15's holder is removed"))
	assert.Equal(t, int64(0), f.count(t, run.shadow.ShadowTable(), "id = -5"), "the row that arrived after the chunk settled is the catch-up's")
	assert.Equal(t, int64(1), f.count(t, run.shadow.ShadowTable(), "id = 15"))
}

// A source row whose key the shadow already holds is not held off when
// another row of the chunk collides: with slot 15 held under key 2000 and
// key 20 already in the shadow with its own slot, only key 15 is pending.
// Were key 20 reported pending, the chunk could never land, since no pass
// would ever insert it.
func TestCopierDoesNotHoldOffARowTheShadowAlreadyHolds(t *testing.T) {
	f := newCopierFixture(t)
	run := f.startSeats(t, 6, 10*time.Millisecond, map[int64]int64{2000: 15, 20: 20})
	run.awaitPending(t, 1)
	assert.Equal(t, []int64{15}, run.c.Position().Pending[0].Keys, "key 20 is in the shadow already")
	f.exec(t, "DELETE FROM "+run.name+" WHERE id = 2000")

	const stopDeadline = 30 * time.Second
	require.NoError(t, awaitRun(t, run.results, stopDeadline, "copy after key 15's holder is removed"))
	f.assertConverged(t, run.shadow)
}

// Each pass over the pending keys waits one backoff longer than the one
// before, so a copy that exhausts five passes has waited at least one, two,
// and three backoffs before passes 3, 4, and 5. The bound is a lower one: it
// fails only when a wait is too short, which is what gives the catch-up's
// flush time to move the colliding value.
func TestCopierCollisionBackoffGrowsByOneBackoffPerPass(t *testing.T) {
	f := newCopierFixture(t)
	const backoff = 100 * time.Millisecond
	run := f.startSeats(t, 5, backoff, map[int64]int64{2000: 15})
	const stopDeadline = 30 * time.Second
	require.Error(t, awaitRun(t, run.results, stopDeadline, "copy that exhausts its passes"))
	assert.GreaterOrEqual(t, time.Since(*run.began), 6*backoff, "waits of one, two, and three backoffs precede passes 3, 4, and 5")
}
