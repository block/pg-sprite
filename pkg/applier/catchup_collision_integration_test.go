package applier_test

import (
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/applier"
	"github.com/block/pg-sprite/pkg/copier"
)

// A source change moves a unique value from a landed key to a key the copier
// has yet to copy, and the catch-up's flushes are held, so the landed row's
// shadow copy keeps the value when the copier reaches the other key: the
// chunk carrying it collides on the shadow's unique index. The copier runs
// the chunk again after a wait, the resumed catch-up flushes the landed
// row's half of the move, the next attempt lands, and the shadow converges
// (CO-4). Holding the flushes until the collision is seen makes the race
// happen on every run rather than when the timing allows it.
func TestCatchupConvergesWhenTheCopierCollidesWithAnUnflushedMove(t *testing.T) {
	f := newCatchupFixture(t, 1500)
	// The seeded rows hold uniq = id. Once the copier has landed key 10,
	// uniq 10 moves to key 1000, far past the chunks in flight then.
	move := &moveOnLanding{
		t:      t,
		f:      f,
		landed: 10,
		statements: []string{
			`UPDATE %s SET uniq = 1000000 WHERE id = 10`,
			`UPDATE %s SET uniq = 10 WHERE id = 1000`,
		},
	}
	run := startCatchupWith(t, f, catchupSetup{copierClock: move})
	move.shadow = pgx.Identifier{f.table.Schema, run.shadow.ShadowTable()}.Sanitize()

	resume, err := run.catchup.Pause(t.Context())
	require.NoError(t, err)
	t.Cleanup(resume)

	copied := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() { copied <- run.copier.Run(t.Context(), f.pool) })
	t.Cleanup(wg.Wait)

	const collisionDeadline = 15 * time.Second
	require.Eventually(t, func() bool { return run.copier.Collisions() >= 1 }, collisionDeadline, 10*time.Millisecond,
		"the chunk carrying key 1000 should collide on uniq 10, which landed key 10 still holds in the shadow")
	require.NoError(t, move.result())

	resume()
	const copyDeadline = 30 * time.Second
	select {
	case err := <-copied:
		require.NoError(t, err, "the collided chunk lands once the flush has moved uniq 10 off key 10")
	case <-time.After(copyDeadline):
		require.FailNow(t, "the copy did not finish after the flushes resumed")
	}
	assert.Equal(t, copier.NewWatermark(math.MaxInt64), run.copier.Position().Cut, "every key is landed")
	run.awaitStatus(t, "flush the move's half on landed key 10", func(s applier.Status) bool {
		return s.Applied > 0 && s.Buffered == 0
	})

	run.stop(t)
	f.assertConverged(t, run.shadow)
}

// moveOnLanding is the copier's clock. On the first reading after the shadow
// holds key landed, it commits statements against the source in one
// transaction, once; each statement's %s is the source table. Every reading
// returns the wall clock.
type moveOnLanding struct {
	t          *testing.T
	f          catchupFixture
	landed     int64
	statements []string
	// shadow is the quoted shadow table, set before the copier runs.
	shadow string

	mu    sync.Mutex
	moved bool
	err   error
}

func (m *moveOnLanding) Now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.moved && m.err == nil {
		m.moved, m.err = m.tryMove()
	}
	return time.Now()
}

func (m *moveOnLanding) tryMove() (bool, error) {
	ctx := m.t.Context()
	var landed bool
	if err := m.f.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM "+m.shadow+" WHERE id = $1)", m.landed).Scan(&landed); err != nil {
		return false, fmt.Errorf("read whether key %d has landed: %w", m.landed, err)
	}
	if !landed {
		return false, nil
	}
	err := pgx.BeginFunc(ctx, m.f.pool, func(tx pgx.Tx) error {
		for _, statement := range m.statements {
			if _, err := tx.Exec(ctx, fmt.Sprintf(statement, m.f.table.Qualified())); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("move a unique value off landed key %d: %w", m.landed, err)
	}
	return true, nil
}

// result reports whether the move was committed.
func (m *moveOnLanding) result() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	if !m.moved {
		return fmt.Errorf("key %d never landed, so the move was not made", m.landed)
	}
	return nil
}
