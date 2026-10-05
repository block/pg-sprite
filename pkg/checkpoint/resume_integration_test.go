package checkpoint_test

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/statement"
)

// copyFixture puts the real shadow builder and copier in front of the
// store, in a throwaway schema of the store's database, so the resume
// below goes through the same objects a run would checkpoint.
type copyFixture struct {
	storeFixture
	cfg    dbconn.Config
	schema string
}

func newCopyFixture(t *testing.T) copyFixture {
	t.Helper()
	f := newStoreFixture(t)
	f.ensure(t)
	return copyFixture{storeFixture: f, cfg: dbconn.Config{URL: f.url}, schema: testutil.NewSchema(t, f.pool)}
}

// exec runs SQL with %s standing for the fixture schema.
func (f copyFixture) exec(t *testing.T, sql string) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), strings.ReplaceAll(sql, "%s", f.schema))
	require.NoError(t, err)
}

// prepare creates an orders table holding keys 1..rows, proves it, takes
// its table lock, and builds the shadow that drops the note column.
func (f copyFixture) prepare(t *testing.T, rows int64) (preflight.CopySwapTarget, *dbconn.TableLockSession, schemachange.BuiltShadow) {
	t.Helper()
	f.exec(t, `
		CREATE TABLE %s.orders (
			id bigint PRIMARY KEY,
			qty integer NOT NULL,
			note text
		)`)
	f.exec(t, fmt.Sprintf(`
		INSERT INTO %%s.orders (id, qty, note)
		SELECT n, n, 'order ' || n FROM generate_series(1, %d) AS n`, rows))

	// The volume is not measured in tests; an unbounded free-disk figure
	// admits the environment check so the proof under test is the shape.
	target, err := preflight.CheckCopySwap(t.Context(), f.pool, f.schema, "orders", preflight.CopySwapEnvironment{FreeDiskBytes: math.MaxInt64})
	require.NoError(t, err)

	lock, err := dbconn.AcquireTableLock(t.Context(), f.cfg, f.schema, "orders")
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, lock.Release(context.WithoutCancel(t.Context()))) })

	alter, err := statement.ParseOne(fmt.Sprintf(`ALTER TABLE %s DROP COLUMN note`, pgx.Identifier{f.schema, "orders"}.Sanitize()))
	require.NoError(t, err)
	shadow, err := schemachange.BuildShadow(t.Context(), f.pool, lock, target, alter, schemachange.Options{})
	require.NoError(t, err)
	return target, lock, shadow
}

// hookedClock runs hook on its first reading. The copier reads its clock
// after the resume clear and the first claim, before the first chunk
// transaction, so a hook there meets the copy as a concurrent writer.
type hookedClock struct {
	once sync.Once
	hook func()
}

func (c *hookedClock) Now() time.Time {
	c.once.Do(c.hook)
	return time.Now()
}

// pinShadowKey inserts key into the shadow in a transaction left open at
// the copier's first clock reading, so the chunk holding key waits on the
// unique index and every other chunk lands around it; the run is then
// cancelled with that one chunk in flight, which is the mid-copy kill the
// checkpoint must survive.
func (f copyFixture) pinShadowKey(t *testing.T, shadow schemachange.BuiltShadow, key int64) (clock *hookedClock, pinned chan struct{}, release func()) {
	t.Helper()
	pinned = make(chan struct{})
	var tx pgx.Tx
	var pinErr error
	clock = &hookedClock{hook: func() {
		defer close(pinned)
		tx, pinErr = f.pool.Begin(t.Context())
		if pinErr != nil {
			return
		}
		_, pinErr = tx.Exec(t.Context(), "INSERT INTO "+pgx.Identifier{f.schema, shadow.ShadowTable()}.Sanitize()+" (id, qty) VALUES ($1, 0)", key)
	}}
	var once sync.Once
	release = func() {
		once.Do(func() {
			<-pinned
			require.NoError(t, pinErr)
			assert.NoError(t, tx.Rollback(context.WithoutCancel(t.Context())))
		})
	}
	t.Cleanup(release)
	return clock, pinned, release
}

// A copy killed mid-way resumes from the checkpoint and converges: the
// first run's committed watermark is saved with the shadow's fingerprints,
// a new run loads it with the same fingerprints, hands it to the copier,
// and the copier clears above it and copies the rest — including a change
// made above the watermark while nothing ran — until the shadow equals the
// source. The checkpoint recorded only committed work: the resumed copier
// inserts exactly the rows above the watermark, including the pinned chunk
// that never landed.
func TestResumeFromCheckpointConvergesAfterAMidCopyKill(t *testing.T) {
	f := newCopyFixture(t)
	const rows = 2000
	target, lock, shadow := f.prepare(t, rows)
	clock, pinned, release := f.pinShadowKey(t, shadow, 1050)

	first, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{
		Workers:     4,
		LockTimeout: 30 * time.Second,
		Chunker:     copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
		Clock:       clock,
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	results := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() { results <- first.Run(ctx, f.pool) })
	t.Cleanup(wg.Wait)

	const pinDeadline = 15 * time.Second
	select {
	case <-pinned:
	case <-time.After(pinDeadline):
		t.Fatalf("the copier did not reach its first chunk within %s", pinDeadline)
	}
	const landedDeadline = 15 * time.Second
	require.Eventually(t, func() bool {
		pos := first.Position()
		return pos.Cut == copier.NewWatermark(math.MaxInt64) && len(pos.InFlight) == 1
	}, landedDeadline, 20*time.Millisecond, "every chunk but the pinned one should land")
	cancel()
	const stopDeadline = 15 * time.Second
	select {
	case err := <-results:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(stopDeadline):
		t.Fatalf("copy did not stop within %s of cancellation", stopDeadline)
	}
	killed := first.Position()
	require.Equal(t, copier.NewWatermark(1000), killed.Watermark, "the watermark stops below the pinned chunk")

	// The run checkpoints what it committed, keyed on the shadow it built.
	require.NoError(t, f.store.Save(t.Context(), lock, checkpoint.Checkpoint{
		Schema:            target.Schema(),
		Table:             target.Table(),
		ShadowTable:       shadow.ShadowTable(),
		Watermark:         killed.Watermark,
		SourceFingerprint: shadow.SourceFingerprint(),
		TargetFingerprint: shadow.TargetFingerprint(),
		Phase:             checkpoint.PhaseCopying,
	}))
	release()

	// While nothing runs, the source moves above the watermark.
	f.exec(t, "UPDATE %s.orders SET qty = 999 WHERE id = 1500")

	// A new run resumes from the row, under the same fingerprints.
	loaded, err := f.store.Load(t.Context(), target.Schema(), target.Table(),
		checkpoint.Fingerprints{Source: shadow.SourceFingerprint(), Target: shadow.TargetFingerprint()})
	require.NoError(t, err)
	assert.Equal(t, copier.NewWatermark(1000), loaded.Watermark)
	assert.Equal(t, shadow.ShadowTable(), loaded.ShadowTable)
	assert.Equal(t, checkpoint.PhaseCopying, loaded.Phase)

	resumed, err := copier.NewCopier(target, shadow, lock, loaded.Watermark, copier.Options{Workers: 2})
	require.NoError(t, err)
	require.NoError(t, resumed.Run(t.Context(), f.pool))
	testutil.AssertConverged(t, f.pool,
		testutil.RelationRef{Schema: f.schema, Table: shadow.SourceTable()},
		testutil.RelationRef{Schema: f.schema, Table: shadow.ShadowTable()},
		testutil.ConvergeOptions{IgnoreColumns: []string{"note"}})
	assert.Equal(t, int64(rows-1000), resumed.Position().RowsInserted, "everything above the watermark was cleared and copied again")

	// The completed copy checkpoints the complete watermark over the same row.
	require.NoError(t, f.store.Save(t.Context(), lock, checkpoint.Checkpoint{
		Schema:            target.Schema(),
		Table:             target.Table(),
		ShadowTable:       shadow.ShadowTable(),
		Watermark:         resumed.Position().Watermark,
		SourceFingerprint: shadow.SourceFingerprint(),
		TargetFingerprint: shadow.TargetFingerprint(),
		Phase:             checkpoint.PhaseVerifying,
	}))
	final, err := f.store.Load(t.Context(), target.Schema(), target.Table(), loaded.Fingerprints())
	require.NoError(t, err)
	assert.True(t, final.Watermark.Complete())
	assert.Equal(t, checkpoint.PhaseVerifying, final.Phase)
	assert.Equal(t, int64(1), f.rowCount(t))
}
