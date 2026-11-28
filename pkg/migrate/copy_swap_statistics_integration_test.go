package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/verdict"
)

// createUnvacuumedOrders is the table the statistics tests change:
// autovacuum is off for it, so nothing but the run itself can give the
// swapped-in table statistics while a test looks, and the swap carries the
// storage parameter over to the new table. Each placed_on date has ten
// rows, so a lookup of one date has a known answer to estimate. The source
// is analyzed, as a production table is: the run must leave the new table
// no worse off than the one it replaces.
func (f copySwapFixture) createUnvacuumedOrders(t *testing.T) {
	t.Helper()
	f.exec(t, `
		CREATE TABLE {schema}.orders (
			id        bigint PRIMARY KEY,
			amount    integer NOT NULL,
			placed_on date NOT NULL
		) WITH (autovacuum_enabled = false)`)
	f.exec(t, `CREATE INDEX orders_placed_on_idx ON {schema}.orders (placed_on)`)
	f.exec(t, `CREATE STATISTICS {schema}.orders_amount_placed_on_stat ON amount, placed_on FROM {schema}.orders`)
	f.exec(t, `
		INSERT INTO {schema}.orders (id, amount, placed_on)
		SELECT g, g % 100, date '2026-01-01' + (g % 500)
		FROM generate_series(1, 5000) g`)
	f.exec(t, `ANALYZE {schema}.orders`)
}

// reltuples is the planner's row count for table: -1 for a table neither
// ANALYZE nor VACUUM has visited.
func (f copySwapFixture) reltuples(t *testing.T, table string) float64 {
	t.Helper()
	var n float64
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT c.reltuples FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2`, f.schema, table).Scan(&n))
	return n
}

// analyzedColumns lists the columns of table that pg_stats has statistics
// for, sorted.
func (f copySwapFixture) analyzedColumns(t *testing.T, table string) []string {
	t.Helper()
	rows, err := f.pool.Query(t.Context(), `
		SELECT attname FROM pg_stats WHERE schemaname = $1 AND tablename = $2 ORDER BY attname`, f.schema, table)
	require.NoError(t, err)
	columns, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	return columns
}

// extendedStatisticsBuilt reports whether pg_stats_ext has data for the
// statistics object name on table.
func (f copySwapFixture) extendedStatisticsBuilt(t *testing.T, table, name string) bool {
	t.Helper()
	var built bool
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT EXISTS (
			SELECT 1 FROM pg_stats_ext
			WHERE schemaname = $1 AND tablename = $2 AND statistics_name = $3 AND n_distinct IS NOT NULL)`,
		f.schema, table, name).Scan(&built))
	return built
}

// estimatedRows is the planner's row estimate for query, read from EXPLAIN.
func (f copySwapFixture) estimatedRows(t *testing.T, query string) float64 {
	t.Helper()
	var plan []struct {
		Plan struct {
			Rows float64 `json:"Plan Rows"`
		} `json:"Plan"`
	}
	var raw []byte
	require.NoError(t, f.pool.QueryRow(t.Context(), "EXPLAIN (FORMAT JSON) "+f.inSchema(query)).Scan(&raw))
	require.NoError(t, json.Unmarshal(raw, &plan))
	require.Len(t, plan, 1)
	return plan[0].Plan.Rows
}

// assertSwappedTableAnalyzed is the oracle both runs' successes share:
// the table under the source's name is the relation the run built as its
// shadow, and it has every statistic the source had, right after the run
// returns, with autovacuum off — a row count, per-column statistics for
// the retyped column and the others, the extended statistics object's
// data under the name it took over — so a lookup of one date is estimated
// at about the ten rows it returns.
func assertSwappedTableAnalyzed(t *testing.T, f copySwapFixture) {
	t.Helper()
	var proof schemachange.Proof
	require.NoError(t, json.Unmarshal(f.inspectRow(t).ShadowProof, &proof))
	var liveOID uint32
	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT $1::regclass::oid`, pgx.Identifier{f.schema, "orders"}.Sanitize()).Scan(&liveOID))
	assert.Equal(t, proof.ShadowOID, liveOID, "the table under the source's name is the relation the run analyzed as its shadow")

	assert.InDelta(t, 5000, f.reltuples(t, "orders"), 1, "the new table has a row count right after the swap")
	assert.Equal(t, []string{"amount", "id", "placed_on"}, f.analyzedColumns(t, "orders"),
		"the retyped column and every other column have statistics right after the swap")
	assert.True(t, f.extendedStatisticsBuilt(t, "orders", "orders_amount_placed_on_stat"),
		"the extended statistics object the new table took over has data")
	assert.InDelta(t, 10, f.estimatedRows(t, `SELECT * FROM {schema}.orders WHERE placed_on = date '2026-03-01'`), 5)
	var reloptions []string
	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT reloptions FROM pg_class WHERE oid = $1`, liveOID).Scan(&reloptions))
	assert.Equal(t, []string{"autovacuum_enabled=false"}, reloptions, "the swap keeps autovacuum off on the new table")
}

// A quiesced copy-and-swap retype analyzes its shadow before the swap, so
// the table it moves in under the source's name has the planner
// statistics it needs the moment the swap commits, without waiting on
// autovacuum, which is off for this table and would never supply them.
// The verdict reports the refresh as the native retype path does.
func TestCopyAndSwapRetypeLeavesTheNewTableAnalyzed(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createUnvacuumedOrders(t)

	v, err := f.run(t, `ALTER TABLE {schema}.orders ALTER COLUMN amount TYPE bigint`, f.options())
	require.NoError(t, err)

	assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome)
	require.NotNil(t, v.StatisticsRefresh)
	assert.Equal(t, verdict.StatisticsRefresh{Columns: []string{"amount"}, Refreshed: true}, *v.StatisticsRefresh)
	assert.Contains(t, v.Detail, "the new table was analyzed before the swap")
	assertSwappedTableAnalyzed(t, f)
}

// A live run analyzes its shadow too, while its capture still applies the
// stream, and the table it swaps in has statistics right after the run.
func TestCopyAndSwapLiveRetypeLeavesTheNewTableAnalyzed(t *testing.T) {
	serverURL := testutil.StartPostgresWithSettings(t, "wal_level=logical")
	cfg := dbconn.Config{URL: testutil.NewDatabase(t, serverURL), MaxConnsFloor: CopySwapMinConns(copier.Options{})}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	f := copySwapFixture{cfg: cfg, pool: pool, schema: testutil.NewSchema(t, pool)}
	f.createUnvacuumedOrders(t)
	opts := f.options()
	opts.CopySwap.BlockWrites = ""

	v, err := f.run(t, `ALTER TABLE {schema}.orders ALTER COLUMN amount TYPE bigint`, opts)
	require.NoError(t, err)

	assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome)
	assert.False(t, v.WritesBlocked)
	require.NotNil(t, v.StatisticsRefresh)
	assert.Equal(t, verdict.StatisticsRefresh{Columns: []string{"amount"}, Refreshed: true}, *v.StatisticsRefresh)
	assertSwappedTableAnalyzed(t, f)
}

// An ANALYZE that cannot run does not stop the swap: the shadow's rows are
// verified either way, and statistics are the planner's, not the data's.
// Here another session holds SHARE UPDATE EXCLUSIVE on the shadow, as a
// VACUUM would, from the verifying save until the cutover save, so the
// ANALYZE waits out its lock budget and gives up. The run still swaps and
// reports executed, and the verdict names the ANALYZE still to run; the
// new table has no row count, which shows the fault was real, since
// autovacuum is off for it and nothing else analyzed it.
func TestCopyAndSwapSwapsWhenTheShadowCannotBeAnalyzed(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createUnvacuumedOrders(t)
	holder, err := pgx.Connect(t.Context(), f.cfg.URL)
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := holder.Close(context.WithoutCancel(t.Context())); err != nil {
			t.Logf("close holder: %v", err)
		}
	})
	shadow := pgx.Identifier{f.schema, schemachange.ShadowName(f.schema, "orders")}.Sanitize()
	var held pgx.Tx
	opts := f.options()
	opts.CopySwap.OnCheckpoint = func(ctx context.Context, cp checkpoint.Checkpoint) error {
		switch cp.Phase {
		case checkpoint.PhaseVerifying:
			tx, err := holder.Begin(ctx)
			if err != nil {
				return err
			}
			held = tx
			_, err = tx.Exec(ctx, "LOCK TABLE "+shadow+" IN SHARE UPDATE EXCLUSIVE MODE")
			return err
		case checkpoint.PhaseCutover:
			if held == nil {
				return errors.New("the run saved cutover without saving verifying first")
			}
			return held.Rollback(ctx)
		default:
			return nil
		}
	}

	v, err := f.run(t, `ALTER TABLE {schema}.orders ALTER COLUMN amount TYPE bigint`, opts)
	require.NoError(t, err)

	assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome)
	assert.Equal(t, "bigint", f.columnType(t, "orders", "amount"), "the swap committed")
	require.NotNil(t, v.StatisticsRefresh)
	assert.Equal(t, verdict.StatisticsRefresh{
		Columns: []string{"amount"},
		Pending: `ANALYZE ` + pgx.Identifier{f.schema, "orders"}.Sanitize(),
	}, *v.StatisticsRefresh)
	assert.Contains(t, v.Detail, "has no planner statistics: run ANALYZE")
	assert.EqualValues(t, -1, f.reltuples(t, "orders"), "the new table was never analyzed")
	assert.Empty(t, f.analyzedColumns(t, "orders"))
}
