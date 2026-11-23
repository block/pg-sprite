package checksum

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/copier"
)

// No pass leaves more than MaxCarryKeys keys for the swap, so only a
// forged proof names more, and the compare refuses one as an invariant
// violation before it runs a statement: the bound on the keys is the bound
// on what the swap reads under ACCESS EXCLUSIVE (CO-1). The proof here is
// built by hand over two real, empty tables the transaction holds locked,
// so the compare would otherwise run to the end: exactly MaxCarryKeys
// keys are compared, every one absent from both sides and so equal. The
// proof one key over is handed a transaction already rolled back, which
// can run no statement, so only a refusal made before the compare touches
// the server names the key count.
func TestCompareCarriedRefusesAProofNamingMoreKeysThanAPassMayLeave(t *testing.T) {
	f := newProofFixture(t)
	f.exec(t, `
		CREATE TABLE %s.orders (
			id bigint PRIMARY KEY,
			qty integer NOT NULL
		)`)
	f.exec(t, `
		CREATE TABLE %s.orders_new (
			id bigint PRIMARY KEY,
			qty integer NOT NULL
		)`)
	target := f.prove(t, "orders")
	source, held := pgx.Identifier{f.schema, "orders"}.Sanitize(), pgx.Identifier{f.schema, "orders_new"}.Sanitize()
	shadow := fakeShadow{schema: f.schema, source: "orders", shadow: "orders_new", columns: []string{"id", "qty"}}
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT $1::regclass::oid, $2::regclass::oid`, source, held).Scan(&shadow.sourceOID, &shadow.shadowOID))
	tx, err := f.pool.BeginTx(t.Context(), pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, tx.Rollback(context.WithoutCancel(t.Context()))) })
	_, err = tx.Exec(t.Context(), "LOCK TABLE "+source+", "+held+" IN ACCESS EXCLUSIVE MODE")
	require.NoError(t, err)
	carrying := func(n int) VerifiedShadow {
		verified := newVerifiedShadow(shadow, copier.NewWatermark(math.MaxInt64), time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC), true)
		for key := range int64(n) {
			verified.carried = append(verified.carried, key+1)
		}
		return verified
	}

	differing, err := CompareCarried(t.Context(), tx, target, shadow, carrying(MaxCarryKeys))
	require.NoError(t, err)
	assert.Empty(t, differing, "a key neither side holds is equal")

	spent, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	require.NoError(t, spent.Rollback(t.Context()))
	differing, err = CompareCarried(t.Context(), spent, target, shadow, carrying(MaxCarryKeys+1))
	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.ErrorContains(t, err, "(CO-1)")
	assert.ErrorContains(t, err, "leaves 1025 keys for the swap, more than the 1024 a pass may leave")
	assert.Empty(t, differing)
}
