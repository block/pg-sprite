package checksum_test

import (
	"context"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/copier"
)

// When a later chunk's repair does not take, the chunks recopied with it
// are still reported: every recopy committed in the one repair
// transaction, so the Outcome lists both repairs while the error names the
// one whose fresh read still differed. The trigger zeroes only keys above
// 1000, so the first chunk's recopy takes and the second's does not.
func TestCheckReportsCommittedRepairsWhenALaterRepairFails(t *testing.T) {
	f := newVerifierFixture(t)
	target, lock, shadow := f.prepare(t)
	f.exec(t, "DELETE FROM "+f.shadowName(shadow)+" WHERE id = 5")
	f.exec(t, "UPDATE "+f.shadowName(shadow)+" SET qty = 0 WHERE id = 1500")
	f.exec(t, `
		CREATE FUNCTION %s.zero_qty() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			NEW.qty := 0;
			RETURN NEW;
		END
		$$`)
	f.exec(t, "CREATE TRIGGER zero_qty BEFORE INSERT ON "+f.shadowName(shadow)+" FOR EACH ROW WHEN (NEW.id > 1000) EXECUTE FUNCTION %s.zero_qty()")

	outcome, err := f.check(t, f.pool, target, shadow, lock, copier.NewWatermark(math.MaxInt64), checksum.DivergenceRepair)
	var failed *checksum.RepairError
	require.ErrorAs(t, err, &failed)
	assert.Equal(t, chunk(t, 1001, 2000), failed.Repair.Mismatch.Chunk)

	require.Len(t, outcome.Report.Mismatches, 2, "the comparison that led to the repairs is reported")
	require.Len(t, outcome.Repairs, 2, "both recopies committed and both are reported")
	assert.Equal(t, chunk(t, math.MinInt64, 1000), outcome.Repairs[0].Mismatch.Chunk)
	assert.Equal(t, int64(999), outcome.Repairs[0].Removed)
	assert.Equal(t, int64(1000), outcome.Repairs[0].Inserted)
	assert.Equal(t, failed.Repair, outcome.Repairs[1], "the repair the error names is the second one reported")
	_, exists := f.shadowRow(t, shadow, 5)
	assert.True(t, exists, "the first chunk's repair committed: row 5 is back")
	assertNoProofs(t, outcome)
}

// Under DivergenceRepair the shadow converges when the source moved a
// unique value from a row in one chunk to a row in another while the
// shadow stood still, the shape slot loss leaves behind. The shadow
// carries the source's unique index, so recopying one chunk at a time
// would insert key 100 with the value key 1500 still holds in the shadow
// and the index would refuse it on every pass; deleting both chunks before
// putting either back lets both land.
func TestCheckRepairsAUniqueValueThatMovedBetweenChunks(t *testing.T) {
	f := newVerifierFixture(t)
	f.createOrders(t)
	f.exec(t, "CREATE UNIQUE INDEX orders_qty ON %s.orders (qty)")
	target := f.prove(t, "orders")
	lock := f.lock(t, "orders")
	shadow := f.build(t, lock, target, `ALTER TABLE %s DROP COLUMN note`)
	f.copy(t, target, shadow, lock)
	f.exec(t, "UPDATE %s.orders SET qty = -1 WHERE id = 100")
	f.exec(t, "UPDATE %s.orders SET qty = 100 WHERE id = 1500")
	f.exec(t, "UPDATE %s.orders SET qty = 1500 WHERE id = 100")

	outcome, err := f.check(t, f.pool, target, shadow, lock, copier.NewWatermark(math.MaxInt64), checksum.DivergenceRepair)
	require.NoError(t, err)
	require.Len(t, outcome.Repairs, 2)
	assert.Equal(t, chunk(t, math.MinInt64, 1000), outcome.Repairs[0].Mismatch.Chunk)
	assert.Equal(t, chunk(t, 1001, 2000), outcome.Repairs[1].Mismatch.Chunk)
	qty, _ := f.shadowRow(t, shadow, 100)
	assert.Equal(t, int64(1500), qty, "key 100 holds the value that moved to it")
	qty, _ = f.shadowRow(t, shadow, 1500)
	assert.Equal(t, int64(100), qty, "key 1500 holds the value that moved to it")
	assertNoProofs(t, outcome)

	again, err := f.check(t, f.pool, target, shadow, lock, copier.NewWatermark(math.MaxInt64), checksum.DivergenceRepair)
	require.NoError(t, err)
	assert.True(t, again.Clean(), "the fresh pass finds the shadow equal to the source")
}

// A shadow replaced between the comparison pass and the repair is refused
// by the repair transaction's own guard before it writes (ST-6): the
// impostor that now carries the shadow's name receives no rows, the
// displaced shadow keeps the difference the pass found, and nothing is
// reported as repaired. The repair is the pass's only read-write
// transaction, so its begin is where the swap is injected.
func TestCheckRepairRefusesAReplacedShadowBeforeWriting(t *testing.T) {
	f := newVerifierFixture(t)
	target, lock, shadow := f.prepare(t)
	f.exec(t, "UPDATE "+f.shadowName(shadow)+" SET qty = 0 WHERE id = 1500")
	name := f.shadowName(shadow)
	pool := f.poolHookedBeforeStatement(t, "begin read write", func() {
		f.exec(t, "ALTER TABLE "+name+" RENAME TO displaced")
		f.exec(t, "CREATE TABLE "+name+" (LIKE %s.displaced INCLUDING ALL)")
	})

	outcome, err := f.check(t, pool, target, shadow, lock, copier.NewWatermark(math.MaxInt64), checksum.DivergenceRepair)
	require.ErrorIs(t, err, checksum.ErrInvariantViolation)
	assert.Contains(t, err.Error(), "(ST-6)")
	assert.Empty(t, outcome.Repairs, "nothing was recopied")
	var impostorRows int64
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+name).Scan(&impostorRows))
	assert.Zero(t, impostorRows, "the repair wrote nothing into the impostor")
	var displacedQty int64
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT qty FROM "+pgx.Identifier{f.schema, "displaced"}.Sanitize()+" WHERE id = 1500").Scan(&displacedQty))
	assert.Equal(t, int64(0), displacedQty, "the displaced shadow was not repaired either")
}

// A source row in a differing chunk that changes inside the pass — after
// the recopy read it and before the fresh read compares it — makes the
// repaired chunk differ again, and the pass refuses it as a RepairError
// rather than repair it twice: a repair pass assumes the source rows it
// recopies hold still, which the reconciliation after slot loss arranges
// by running before change capture resumes. The recopy itself took and is
// reported; the write is injected as the recopy's insert completes.
func TestCheckRepairRefusesASourceWriteInsideThePass(t *testing.T) {
	f := newVerifierFixture(t)
	target, lock, shadow := f.prepare(t)
	f.exec(t, "UPDATE "+f.shadowName(shadow)+" SET qty = 0 WHERE id = 1500")
	var once sync.Once
	pool := f.hookedPool(t, func(_ context.Context, _ *pgx.Conn, sql string) {
		if strings.HasPrefix(sql, "INSERT INTO") {
			once.Do(func() { f.exec(t, "UPDATE %s.orders SET qty = qty + 1 WHERE id = 1999") })
		}
	})

	outcome, err := f.check(t, pool, target, shadow, lock, copier.NewWatermark(math.MaxInt64), checksum.DivergenceRepair)
	var failed *checksum.RepairError
	require.ErrorAs(t, err, &failed)
	assert.Equal(t, chunk(t, 1001, 2000), failed.After.Chunk)
	assert.Equal(t, int64(1000), failed.After.Source.Rows)
	assert.Equal(t, int64(1000), failed.After.Shadow.Rows, "the rows are all there; one value differs")
	assert.Equal(t, []checksum.Repair{failed.Repair}, outcome.Repairs, "the recopy committed and is reported")
	qty, _ := f.shadowRow(t, shadow, 1500)
	assert.Equal(t, int64(1500), qty, "the recopy took")
	qty, _ = f.shadowRow(t, shadow, 1999)
	assert.Equal(t, int64(1999), qty, "the shadow holds the row as the recopy read it, not the later write")
	assertNoProofs(t, outcome)
}
