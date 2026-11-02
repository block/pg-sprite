package applier_test

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/applier"
	"github.com/block/pg-sprite/pkg/copier"
)

// A bytea converted to text lands the same text from the swap's drain as
// from the copy, whatever bytea_output the caller's transaction carries:
// the server renders the bytes \x01ff as \x01ff under hex and as \001\377
// under escape, and the drain is the last write before the swap, with no
// verifier after it. The caller's own bytea_output is put back afterwards.
func TestFlushInConvertsAByteaToTextAsTheCopyDoes(t *testing.T) {
	f := newFlushFixture(t)
	f.exec(t, `
		CREATE TABLE %s.blobs (
			id bigint PRIMARY KEY,
			body bytea,
			note text
		)`)
	f.exec(t, `INSERT INTO %s.blobs VALUES (1, '\x01ff'::bytea, 'n'), (2, '\x01ff'::bytea, 'n')`)
	target := f.prove(t, "blobs")
	lock := f.lock(t, "blobs")
	shadow := f.build(t, lock, target, `
		ALTER TABLE %s
			ALTER COLUMN body TYPE text USING body::text`)
	require.Equal(t, map[string]string{"body": "body::text"}, shadow.Conversions())
	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{Workers: 1})
	require.NoError(t, err)
	require.NoError(t, c.Run(t.Context(), f.poolWithRuntimeParam(t, "bytea_output", "escape")))
	copied, _ := f.text(t, shadow.ShadowTable(), "body", 1)
	flush, err := applier.NewFlusher(target, shadow, lock, applier.Options{})
	require.NoError(t, err)
	tx, err := f.pool.BeginTx(t.Context(), pgx.TxOptions{AccessMode: pgx.ReadWrite})
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), `SET LOCAL bytea_output = 'escape'`)
	require.NoError(t, err)

	result, err := flush.FlushIn(t.Context(), tx, batch(image(2, col("body", `\x01ff`), col("note", "n"))))

	require.NoError(t, err)
	assert.Equal(t, applier.Result{Images: 1}, result)
	var output string
	require.NoError(t, tx.QueryRow(t.Context(), `SELECT current_setting('bytea_output')`).Scan(&output))
	assert.Equal(t, "escape", output, "the caller's bytea_output is put back after the flush")
	require.NoError(t, tx.Commit(t.Context()))
	drained, _ := f.text(t, shadow.ShadowTable(), "body", 2)
	require.NotNil(t, copied)
	require.NotNil(t, drained)
	assert.Equal(t, `\x01ff`, *copied, "the copy renders hex whatever its pool is configured with")
	assert.Equal(t, *copied, *drained, "the same source value converted by the copy and by the drain")
}

// A float converted to text lands the same text from the swap's drain as
// from the copy, whatever extra_float_digits the caller's transaction
// carries: at zero the server rounds 0.1 + 0.2 to fifteen significant
// digits, 0.3, while the copy renders every digit, 0.30000000000000004.
// The caller's own extra_float_digits is put back afterwards.
func TestFlushInConvertsAFloatToTextAsTheCopyDoes(t *testing.T) {
	f := newFlushFixture(t)
	f.exec(t, `
		CREATE TABLE %s.readings (
			id bigint PRIMARY KEY,
			value float8,
			note text
		)`)
	f.exec(t, `INSERT INTO %s.readings VALUES (1, 0.1::float8 + 0.2::float8, 'n'), (2, 0.1::float8 + 0.2::float8, 'n')`)
	target := f.prove(t, "readings")
	lock := f.lock(t, "readings")
	shadow := f.build(t, lock, target, `
		ALTER TABLE %s
			ALTER COLUMN value TYPE text USING value::text`)
	require.Equal(t, map[string]string{"value": "value::text"}, shadow.Conversions())
	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{Workers: 1})
	require.NoError(t, err)
	require.NoError(t, c.Run(t.Context(), f.poolWithRuntimeParam(t, "extra_float_digits", "0")))
	copied, _ := f.text(t, shadow.ShadowTable(), "value", 1)
	flush, err := applier.NewFlusher(target, shadow, lock, applier.Options{})
	require.NoError(t, err)
	tx, err := f.pool.BeginTx(t.Context(), pgx.TxOptions{AccessMode: pgx.ReadWrite})
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), `SET LOCAL extra_float_digits = 0`)
	require.NoError(t, err)

	result, err := flush.FlushIn(t.Context(), tx, batch(image(2, col("value", "0.30000000000000004"), col("note", "n"))))

	require.NoError(t, err)
	assert.Equal(t, applier.Result{Images: 1}, result)
	var digits string
	require.NoError(t, tx.QueryRow(t.Context(), `SELECT current_setting('extra_float_digits')`).Scan(&digits))
	assert.Equal(t, "0", digits, "the caller's extra_float_digits is put back after the flush")
	require.NoError(t, tx.Commit(t.Context()))
	drained, _ := f.text(t, shadow.ShadowTable(), "value", 2)
	require.NotNil(t, copied)
	require.NotNil(t, drained)
	assert.Equal(t, "0.30000000000000004", *copied, "the copy renders every digit whatever its pool rounds to")
	assert.Equal(t, *copied, *drained, "the same source value converted by the copy and by the drain")
}

// The output settings a caller sets in its transaction last only for that
// transaction: once it commits, the connection reads the extra_float_digits
// and bytea_output it had before, so a pooled connection hands the next
// transaction its own settings rather than the caller's.
func TestFlushInLeavesTheConnectionsOwnOutputSettings(t *testing.T) {
	f := newFlushFixture(t)
	f.createOrders(t, 10)
	p := f.prepare(t, "orders")
	conn, err := f.pool.Acquire(t.Context())
	require.NoError(t, err)
	t.Cleanup(conn.Release)
	const settings = `SELECT current_setting('extra_float_digits'), current_setting('bytea_output')`
	var digitsBefore, outputBefore string
	require.NoError(t, conn.QueryRow(t.Context(), settings).Scan(&digitsBefore, &outputBefore))
	require.NotEqual(t, "0", digitsBefore, "the caller's value must differ from the connection's")
	require.NotEqual(t, "escape", outputBefore, "the caller's value must differ from the connection's")
	tx, err := conn.BeginTx(t.Context(), pgx.TxOptions{AccessMode: pgx.ReadWrite})
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), `SET LOCAL extra_float_digits = 0; SET LOCAL bytea_output = 'escape'`)
	require.NoError(t, err)

	_, err = p.flush.FlushIn(t.Context(), tx, batch(image(5, col("qty", "500"), col("note", "order 5"))))

	require.NoError(t, err)
	require.NoError(t, tx.Commit(t.Context()))
	var digitsAfter, outputAfter string
	require.NoError(t, conn.QueryRow(t.Context(), settings).Scan(&digitsAfter, &outputAfter))
	assert.Equal(t, digitsBefore, digitsAfter, "the connection's own extra_float_digits outlives the transaction")
	assert.Equal(t, outputBefore, outputAfter, "the connection's own bytea_output outlives the transaction")
}
