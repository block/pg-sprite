package migrate

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/verdict"
)

// newDecodingCopySwapFixture is newCopySwapFixture on a server with logical
// decoding, so the same table can be run either way: live, through a
// replication slot, or with its writes blocked.
func newDecodingCopySwapFixture(t *testing.T) copySwapFixture {
	t.Helper()
	url := testutil.NewDatabase(t, testutil.StartPostgresWithSettings(t, "wal_level=logical", "wal_sender_timeout=2s"))
	cfg := dbconn.Config{URL: url, MaxConnsFloor: CopySwapMinConns(copier.Options{})}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return copySwapFixture{cfg: cfg, pool: pool, schema: testutil.NewSchema(t, pool)}
}

// stopMidCopy runs alter under opts with a copy sized to outlast many
// checkpoint saves, and stops it, as a killed run stops, at the first save
// the copy makes while it is still running: the shadow, the checkpoint row
// in copying, and the run's hold on the source — its write fence, or its
// replication slot — are left standing, and nothing takes them back.
func (f copySwapFixture) stopMidCopy(t *testing.T, alter string, opts Options) {
	t.Helper()
	opts.CopySwap.CheckpointInterval = time.Millisecond
	opts.CopySwap.Copier = copier.Options{
		Workers: 1,
		Chunker: copier.ChunkerOptions{InitialRows: 10, MinRows: 10, MaxRows: 10},
	}
	opts.CopySwap.OnCheckpoint = func(_ context.Context, cp checkpoint.Checkpoint) error {
		if midCopy(cp) {
			return errHostStopped
		}
		return nil
	}
	_, err := f.run(t, alter, opts)
	require.ErrorIs(t, err, errHostStopped, "a save landed while the copy was still running")
	require.Equal(t, checkpoint.PhaseCopying, f.inspectRow(t).Phase)
}

// A --block-writes run of
//
//	ALTER TABLE orders ALTER COLUMN qty TYPE bigint
//
// stopped mid-copy leaves its write fence on orders, and the fence is right
// to stay: it is what keeps the shadow a true copy. The application's write
// is refused with SQLSTATE 55006 and a hint that names the table and both
// ways out: the same migrate with --block-writes, or pg-sprite discard.
// Running the statement again without --block-writes cannot continue the
// stopped run — a live run streams writes the fenced run never captured —
// so it is refused as resume-mode-mismatch, an environmental refusal, not
// as an engine invariant violation; the verdict says the table's writes are
// still blocked and names the flag to add and the discard command. The
// refusal writes nothing, and the advice works: the run with
// --block-writes finishes the change and the table takes writes again.
func TestBlockWritesRunStoppedMidCopyNamesTheWayOut(t *testing.T) {
	f := newDecodingCopySwapFixture(t)
	f.createOrders(t)
	const alter = `
		ALTER TABLE {schema}.orders
			ALTER COLUMN qty TYPE bigint`
	f.stopMidCopy(t, alter, f.options())

	_, err := f.pool.Exec(t.Context(), f.inSchema(`
		INSERT INTO {schema}.orders (qty, note, sku)
		VALUES (1, 'while stopped', 'sku1')`))
	var refused *pgconn.PgError
	require.True(t, errors.As(err, &refused), "the fenced table refuses the write: %v", err)
	assert.Equal(t, schemachange.WriteFenceSQLState, refused.Code)
	table := f.schema + ".orders"
	assert.Equal(t, fmt.Sprintf("A pg-sprite copy-and-swap blocks writes to %[1]s until the run finishes, and the block stays when the run stops or is killed. "+
		"If no run is in progress, finish it by running the same pg-sprite migrate again with --block-writes %[1]s, or abandon it with pg-sprite discard %[1]s.", table),
		refused.Hint)

	live := f.options()
	live.CopySwap.BlockWrites = ""
	v, err := f.run(t, alter, live)
	require.NoError(t, err, "a refusal is a verdict, not an error")
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome, "%s: %s", v.Reason, v.Detail)
	assert.Equal(t, verdict.ReasonCopyAndSwapRefused, v.Reason)
	assert.Equal(t, verdict.ClassEnvironmental, v.Class)
	assert.Equal(t, verdict.Cause(schemachange.CauseResumeModeMismatch), v.Cause)
	assert.True(t, v.WritesBlocked, "the stopped run's fence still refuses every write")
	assert.Equal(t, fmt.Sprintf("a copy-and-swap run of this statement on %[1]s stopped with the table's writes blocked, "+
		"and this run would stream writes through a replication slot instead, so it cannot continue that run; "+
		"finish the stopped run by running the same command again with --block-writes %[1]s added (CopySwapOptions.BlockWrites), "+
		"or abandon it with pg-sprite discard %[1]s (schemachange.Runner.Discard); "+
		"until then the source stays fenced and every write to it fails with SQLSTATE 55006; nothing was executed", table),
		v.Detail, "the detail names the flag to add and the command that abandons the run")
	assert.Equal(t, checkpoint.PhaseCopying, f.inspectRow(t).Phase, "the refused run leaves the row as it found it")
	var slots int
	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM pg_replication_slots WHERE database = current_database()`).Scan(&slots))
	assert.Zero(t, slots, "the refused live run made no slot")

	v, err = f.run(t, alter, f.options())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome, "%s: %s", v.Reason, v.Detail)
	assert.Equal(t, "bigint", f.columnType(t, "orders", "qty"))
	f.exec(t, `
		INSERT INTO {schema}.orders (qty, note, sku)
		VALUES (1, 'after the swap', 'sku1')`)
	assert.EqualValues(t, 2501, f.rowCount(t, "orders"))
}

// The reverse: a live run of the same retype stopped mid-copy leaves its
// replication slot and no fence. Running the statement again with
// --block-writes cannot continue it — the fenced run has no stream to
// replay the writes the live run's slot carries — so it is refused as
// resume-mode-mismatch too, naming the way out: the same command without
// --block-writes, or pg-sprite discard. The verdict does not report the
// table's writes blocked, since nothing fenced it, and the table still
// takes writes; the slot is left for the live run.
func TestLiveRunStoppedMidCopyIsNotContinuedByABlockWritesRun(t *testing.T) {
	f := newDecodingCopySwapFixture(t)
	f.createOrders(t)
	const alter = `
		ALTER TABLE {schema}.orders
			ALTER COLUMN qty TYPE bigint`
	live := f.options()
	live.CopySwap.BlockWrites = ""
	f.stopMidCopy(t, alter, live)

	v, err := f.run(t, alter, f.options())
	require.NoError(t, err, "a refusal is a verdict, not an error")
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome, "%s: %s", v.Reason, v.Detail)
	assert.Equal(t, verdict.ClassEnvironmental, v.Class)
	assert.Equal(t, verdict.Cause(schemachange.CauseResumeModeMismatch), v.Cause)
	assert.False(t, v.WritesBlocked, "a live run fences nothing")
	assert.Contains(t, v.Detail, "without --block-writes")
	assert.Contains(t, v.Detail, "pg-sprite discard "+f.schema+".orders")
	f.exec(t, `
		INSERT INTO {schema}.orders (qty, note, sku)
		VALUES (1, 'while stopped', 'sku1')`)
	row := f.inspectRow(t)
	assert.Equal(t, checkpoint.PhaseCopying, row.Phase, "the refused run leaves the row as it found it")
	_, found, err := decode.InspectSlot(t.Context(), f.pool, row.SlotName)
	require.NoError(t, err)
	assert.True(t, found, "the live run's slot is left for the live run to resume")
}
