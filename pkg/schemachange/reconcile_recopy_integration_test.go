package schemachange_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// The source stays live through a reconcile, so a source transaction can
// move a unique value from a row in a chunk the pass read equal to a row in
// the chunk it is about to recopy, after the comparison and before the
// recopy. The recopy then puts back a row holding a value another shadow
// row still holds and the server refuses it. The reconcile compares again,
// finds both chunks differing, recopies both in one transaction, and
// finishes as it would have without the move: the row resumes in
// catching_up on the new slot and the shadow converges.
func TestReconcileSurvivesAUniqueValueMovedDuringItsRecopy(t *testing.T) {
	f := newReconcileFixture(t, 500)
	slot, err := decode.CreateSlot(t.Context(), f.cfg, f.pool, f.target)
	require.NoError(t, err)
	require.NoError(t, slot.Close(t.Context()))
	t.Cleanup(func() {
		assert.NoError(t, decode.DropSlot(context.WithoutCancel(t.Context()), f.cfg, f.pool, slot.Name()))
	})
	shadow := f.buildShadow(t)
	c := newCopier(t, f, shadow)
	copyAll(t, f, c)
	require.NoError(t, f.store.Save(t.Context(), f.lock, f.row(shadow, checkpoint.PhaseCatchingUp, c.Position().Watermark, slot.Name(), slot.ConsistentPoint())))

	// The slot goes, and the source moves while nothing captures it.
	_, err = f.pool.Exec(t.Context(), `SELECT pg_catalog.pg_drop_replication_slot($1)`, slot.Name())
	require.NoError(t, err)
	f.exec(t, `UPDATE %s SET amount = 0 WHERE id = 150`)

	// Between the pass's comparison and its recopy of the chunk holding key
	// 150, the source moves key 10's unique value to key 160: key 10 lies
	// in a chunk the comparison read equal, key 160 in the chunk being
	// recopied.
	hooked := f.poolHookedBeforeStatement(t, "DELETE FROM", func() {
		f.exec(t, `UPDATE %s SET uniq = -10 WHERE id = 10`)
		f.exec(t, `UPDATE %s SET uniq = 10 WHERE id = 160`)
	})

	saved := f.load(t, shadow)
	reconciler, err := schemachange.NewReconciler(f.cfg, f.lock, f.store, f.target, shadow, checksum.Options{
		Chunker: copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
	})
	require.NoError(t, err)
	reconciled, err := reconciler.Reconcile(t.Context(), hooked, saved)
	require.NoError(t, err)
	require.NotNil(t, reconciled.Slot)
	t.Cleanup(func() { assert.NoError(t, reconciled.Slot.Close(context.WithoutCancel(t.Context()))) })

	assert.Equal(t, 1, reconciled.Outcome.Collisions, "the first recopy collided; the second landed")
	assertRepairsCoverExactly(t, reconciled.Outcome.Repairs, []int64{10, 150, 160}, []int64{300, 500})
	assert.Equal(t, checkpoint.PhaseCatchingUp, reconciled.Checkpoint.Phase)
	assert.Equal(t, checkpoint.PhaseCatchingUp, f.load(t, shadow).Phase)
	f.assertConverged(t, shadow)
}

// poolHookedBeforeStatement is a pool on the fixture's database whose
// connections run hook once, as the first statement starting with prefix
// begins. It is a test-only pool: production code connects through dbconn.
func (f reconcileFixture) poolHookedBeforeStatement(t *testing.T, prefix string, hook func()) *pgxpool.Pool {
	t.Helper()
	pc, err := pgxpool.ParseConfig(f.cfg.URL)
	require.NoError(t, err)
	pc.ConnConfig.Tracer = &statementHook{prefix: prefix, hook: hook}
	pool, err := pgxpool.NewWithConfig(t.Context(), pc)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// statementHook is a pgx query tracer that runs its hook once, before the
// first statement whose text starts with prefix is sent.
type statementHook struct {
	prefix string
	hook   func()
	once   sync.Once
}

func (h *statementHook) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, h.prefix) {
		h.once.Do(h.hook)
	}
	return ctx
}

func (h *statementHook) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
