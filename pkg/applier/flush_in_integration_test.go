package applier_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/applier"
)

// sqlstateInFailedTransaction is what the server raises for any statement
// after one has failed inside the same transaction.
const sqlstateInFailedTransaction = "25P02"

// A batch flushed into a caller's transaction commits with that
// transaction, not before: the shadow does not show the writes until the
// caller commits, and shows every one of them afterwards.
func TestFlushInWritesOnlyWhenTheCallerCommits(t *testing.T) {
	f := newFlushFixture(t)
	f.createOrders(t, 100)
	p := f.prepare(t, "orders")
	f.exec(t, `UPDATE %s.orders SET qty = 500 WHERE id = 5`)
	f.exec(t, `DELETE FROM %s.orders WHERE id = 7`)
	f.exec(t, `INSERT INTO %s.orders (id, qty, note) VALUES (101, 101, 'order 101')`)
	tx, err := f.pool.BeginTx(t.Context(), pgx.TxOptions{AccessMode: pgx.ReadWrite})
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), `SET LOCAL extra_float_digits = 1`)
	require.NoError(t, err)

	result, err := p.flush.FlushIn(t.Context(), tx, batch(
		image(5, col("qty", "500"), col("note", "order 5")),
		deleted(7),
		image(101, col("qty", "101"), col("note", "order 101")),
	))

	require.NoError(t, err)
	assert.Equal(t, applier.Result{Images: 2, Deletes: 1}, result)
	var digits string
	require.NoError(t, tx.QueryRow(t.Context(), `SELECT current_setting('extra_float_digits')`).Scan(&digits))
	assert.Equal(t, "1", digits, "the caller's extra_float_digits is put back after the flush")
	qty, _ := f.text(t, p.shadow.ShadowTable(), "qty", 5)
	assert.Equal(t, "5", *qty, "nothing is visible outside the transaction before it commits")
	_, present := f.text(t, p.shadow.ShadowTable(), "id", 7)
	assert.True(t, present)
	require.NoError(t, tx.Commit(t.Context()))
	f.assertConverged(t, p.shadow)
	assert.Equal(t, int64(100), f.count(t, p.shadow.ShadowTable()))
}

// A batch the shadow refuses even whole-row is deferred and leaves the
// caller's transaction aborted, so nothing the caller did in it can commit
// by accident; after the rollback the shadow is as it was.
func TestFlushInLeavesTheTransactionAbortedWhenTheBatchIsDeferred(t *testing.T) {
	f := newFlushFixture(t)
	f.createSeats(t, 2)
	p := f.prepare(t, "seats")
	doc1, _ := f.text(t, "seats", "doc", 1)
	f.exec(t, `UPDATE %s.seats SET slot = NULL WHERE id = 2`)
	f.exec(t, `UPDATE %s.seats SET slot = 'B' WHERE id = 1`)
	tx, err := f.pool.BeginTx(t.Context(), pgx.TxOptions{AccessMode: pgx.ReadWrite})
	require.NoError(t, err)

	_, err = p.flush.FlushIn(t.Context(), tx, batch(image(1, col("slot", "B"), col("doc", *doc1))))

	require.ErrorIs(t, err, applier.ErrBatchDeferred)
	_, err = tx.Exec(t.Context(), `SELECT 1`)
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "the transaction is aborted: %v", err)
	assert.Equal(t, sqlstateInFailedTransaction, pgErr.Code)
	require.NoError(t, tx.Rollback(t.Context()))
	slot, _ := f.text(t, p.shadow.ShadowTable(), "slot", 1)
	assert.Equal(t, "A", *slot, "the shadow is as it was")
}

// An empty batch touches neither the shadow nor the caller's session
// settings: the search_path the caller set is still in force.
func TestFlushInOfAnEmptyBatchTouchesNothing(t *testing.T) {
	f := newFlushFixture(t)
	f.createOrders(t, 10)
	p := f.prepare(t, "orders")
	tx, err := f.pool.BeginTx(t.Context(), pgx.TxOptions{AccessMode: pgx.ReadWrite})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, tx.Rollback(context.WithoutCancel(t.Context()))) })
	_, err = tx.Exec(t.Context(), `SET LOCAL search_path = `+pgx.Identifier{f.schema}.Sanitize())
	require.NoError(t, err)

	result, err := p.flush.FlushIn(t.Context(), tx, applier.Batch{})

	require.NoError(t, err)
	assert.Equal(t, applier.Result{}, result)
	var path string
	require.NoError(t, tx.QueryRow(t.Context(), `SELECT current_setting('search_path')`).Scan(&path))
	assert.Equal(t, f.schema, path)
}
