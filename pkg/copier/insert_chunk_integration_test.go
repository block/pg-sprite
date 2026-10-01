package copier

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// InsertChunk is the copy statement handed to a caller's transaction: it
// inserts the chunk's source rows that the shadow lacks, leaves the rows
// the shadow already holds as they are (CO-4), reports how many it added,
// and commits with the caller's transaction, not on its own — a rolled-back
// caller leaves the shadow as it was.
func TestInsertChunkRunsTheCopyStatementInTheCallersTransaction(t *testing.T) {
	f := newChunkerFixture(t)
	f.exec(t, `
		CREATE TABLE %s.orders (
			id bigint PRIMARY KEY,
			qty integer NOT NULL
		)`)
	f.exec(t, `INSERT INTO %s.orders (id, qty) SELECT n, n FROM generate_series(1, 10) AS n`)
	f.exec(t, `
		CREATE TABLE %s._pgsprite_orders_new (
			id bigint PRIMARY KEY,
			qty integer NOT NULL
		)`)
	f.exec(t, `INSERT INTO %s._pgsprite_orders_new (id, qty) VALUES (3, 0)`)
	target := f.prove(t, "orders")
	shadow := fakeShadow{
		schema: f.schema, source: "orders", shadow: "_pgsprite_orders_new",
		sourceOID: 1, shadowOID: 2,
		columns: []string{"id", "qty"},
	}
	chunk, err := NewChunk(1, 5)
	require.NoError(t, err)

	rolledBack, err := f.pool.BeginTx(t.Context(), pgx.TxOptions{})
	require.NoError(t, err)
	inserted, err := InsertChunk(t.Context(), rolledBack, target, shadow, chunk)
	require.NoError(t, err)
	assert.Equal(t, int64(4), inserted, "keys 1, 2, 4, 5; key 3 is already held")
	require.NoError(t, rolledBack.Rollback(t.Context()))
	assert.Equal(t, int64(1), f.shadowRows(t), "a rolled-back caller inserted nothing")

	committed, err := f.pool.BeginTx(t.Context(), pgx.TxOptions{})
	require.NoError(t, err)
	inserted, err = InsertChunk(t.Context(), committed, target, shadow, chunk)
	require.NoError(t, err)
	assert.Equal(t, int64(4), inserted)
	require.NoError(t, committed.Commit(t.Context()))
	assert.Equal(t, int64(5), f.shadowRows(t))
	assert.Equal(t, int64(0), f.shadowQty(t, 3), "the held row's value is kept, not overwritten")
}

func (f chunkerFixture) shadowRows(t *testing.T) int64 {
	t.Helper()
	var n int64
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+pgx.Identifier{f.schema, "_pgsprite_orders_new"}.Sanitize()).Scan(&n))
	return n
}

func (f chunkerFixture) shadowQty(t *testing.T, id int64) int64 {
	t.Helper()
	var qty int64
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT qty FROM `+pgx.Identifier{f.schema, "_pgsprite_orders_new"}.Sanitize()+` WHERE id = $1`, id).Scan(&qty))
	return qty
}
