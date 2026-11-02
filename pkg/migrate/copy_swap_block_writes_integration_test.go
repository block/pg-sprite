package migrate

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// A block-writes acknowledgement that names another table is an error before
// any verdict, and the table is never taken offline: no shadow is built,
// no fence goes on, so the source still takes writes, and its shape is
// unchanged. The acknowledgement is what stops a block-writes declaration left
// on a script from fencing the next table it meets.
func TestRunRejectsAMismatchedBlockWritesAck(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	st, err := statement.ParseOne(f.inSchema(`ALTER TABLE {schema}.orders ALTER COLUMN qty TYPE bigint`))
	require.NoError(t, err)

	opts := f.options()
	opts.CopySwap.BlockWrites = f.schema + ".customers"
	v, err := Run(t.Context(), f.pool, st, opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "block-writes acknowledgement")
	assert.Equal(t, verdict.Verdict{}, v, "an error before a verdict carries a zero verdict")
	assert.False(t, f.relationExists(t, schemachange.ShadowName(f.schema, "orders")), "no shadow was built")
	f.exec(t, `INSERT INTO {schema}.orders (qty, note, sku) VALUES (1, 'after', 'sku1')`)
	assert.Equal(t, "integer", f.columnType(t, "orders", "qty"), "nothing was executed")
}

// A blocked-writes run that stops before its fence goes on does not report
// the table as offline. Another session holds the table's lock, so the run
// passes its preflight and then stops at the lock, before it builds a
// shadow or fences the source. The verdict is a failure whose
// writes_blocked is false, because the table kept taking writes, and no
// checkpoint row is left to resume from.
func TestBlockWritesRunStoppedBeforeItsFenceDoesNotReportWritesBlocked(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	holder, err := dbconn.AcquireTableLock(t.Context(), f.cfg, f.schema, "orders")
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, holder.Release(context.WithoutCancel(t.Context()))) })

	v, err := f.run(t, `ALTER TABLE {schema}.orders ALTER COLUMN qty TYPE bigint`, f.options())

	var held *dbconn.TableLockHeldError
	require.ErrorAs(t, err, &held, "the run stops at the lock another session holds")
	assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
	assert.False(t, v.WritesBlocked, "the run stopped before its fence went on")
	store, err := checkpoint.NewStore(f.pool, checkpoint.Options{})
	require.NoError(t, err)
	_, err = store.Inspect(t.Context(), f.schema, "orders")
	assert.True(t, errors.Is(err, checkpoint.ErrNotFound) || errors.Is(err, checkpoint.ErrTableMissing),
		"no checkpoint row was saved: %v", err)
	assert.False(t, f.relationExists(t, schemachange.ShadowName(f.schema, "orders")), "no shadow was built")
	f.exec(t, `INSERT INTO {schema}.orders (qty, note, sku) VALUES (1, 'during', 'sku1')`)
}
