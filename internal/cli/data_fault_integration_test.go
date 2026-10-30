package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/verdict"
)

// dataFaultRows is how many rows the data-fault tables start with: several
// chunks at the copier's initial chunk size, so the one violating row,
// planted in the back half, is met after chunks have already landed in
// the shadow — the abandon has something to discard.
const dataFaultRows = 4000

// dataFaultViolator is the one row's uniq value whose label will not fit
// the narrowed type.
const dataFaultViolator = 3000

// stringTooLong is the SQLSTATE the server raises when a value does not
// fit the column's declared length (string_data_right_truncation).
const stringTooLong = "22001"

// dataFaultLabelWidth is the varchar width the statement narrows label to.
// Every seeded label (row-N) and every label the load writes (insert-N,
// each update appending one character) stays inside it over the length of
// a run; only the planted row, twice as wide, breaks it.
const dataFaultLabelWidth = 32

// A data fault on the front door, with the table's writes blocked: the copy meets one row the
// narrowed type will not hold, part-way through the table, and the run is
// abandoned. The process reports it the way automation reads it — a
// failed verdict on stdout coded data-fault and an operational error, so
// the exit code is 1, not the refusal's 2 — and the server is left as the
// run found it: no shadow, no write fence, no checkpoint row, the source
// its original shape and taking writes. --block-writes keeps the run to the
// write fence, so the test server needs no logical decoding.
func TestMigrateReportsABlockWritesDataFaultAndLeavesNothingBehind(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	table := testutil.NewWorkloadTable(t, pool)
	plantDataFault(t, pool, table)

	cmd := newMigrateCmd(url, dataFaultAlter(table))
	cmd.FreeDisk = byteSize(testutil.UnlimitedDisk)
	cmd.BlockWrites = table.Schema + "." + table.Table
	cmd.JSON = true
	var out strings.Builder
	err = cmd.run(t.Context(), &out)

	assertDataFaultVerdict(t, err, out.String(), table, "the write fence is off the source")
	assertDataFaultLeftNothing(t, pool, table)
}

// The same data fault on a live run: the table is under load, the run holds
// a replication slot and streams the source's changes while it copies, and
// the copy meets the one row the narrowed type will not hold. The abandon
// drops the slot and the publication along with the shadow and the
// checkpoint row, so nothing pins WAL on the server after the verdict, and
// the load — never fenced, since a live run takes no fence — writes the
// source throughout.
func TestMigrateReportsALiveDataFaultAndLeavesNothingBehind(t *testing.T) {
	serverURL := testutil.StartPostgresWithSettings(t, "wal_level=logical", "wal_sender_timeout=2s")
	url := testutil.NewDatabase(t, serverURL)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url, MaxConns: 16})
	require.NoError(t, err)
	defer pool.Close()
	table := testutil.NewWorkloadTable(t, pool)
	plantDataFault(t, pool, table)

	// Inserts and updates only: a delete could take the planted row before
	// the copier reaches it, and the fault would never fire.
	load := testutil.StartLoad(t, pool, table, testutil.LoadSpec{
		Seed: 1, Workers: 2, RatePerSecond: 40,
		Mix:            testutil.Mix{Insert: 2, Update: 3},
		HotRowFraction: 0.2,
	})

	cmd := newMigrateCmd(url, dataFaultAlter(table))
	cmd.FreeDisk = byteSize(testutil.UnlimitedDisk)
	cmd.JSON = true
	var out strings.Builder
	err = cmd.run(t.Context(), &out)

	summary, loadErr := load.Stop()
	require.NoError(t, loadErr, "the load wrote the source throughout: a live run fences nothing")
	assert.Positive(t, summary.Inserts)
	assert.Positive(t, summary.Updates)

	assertDataFaultVerdict(t, err, out.String(), table, "its replication slot is dropped")
	assertDataFaultLeftNothing(t, pool, table)
	var slots int
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_replication_slots`).Scan(&slots))
	assert.Zero(t, slots, "the slot is dropped, so nothing pins WAL")
	var publications int
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_publication`).Scan(&publications))
	assert.Zero(t, publications, "the publication is dropped with the slot")
}

// plantDataFault seeds table and widens one row's label past
// dataFaultLabelWidth.
func plantDataFault(t *testing.T, pool *pgxpool.Pool, table testutil.WorkloadTable) {
	t.Helper()
	require.NoError(t, table.SeedRows(t.Context(), dataFaultRows))
	_, err := pool.Exec(t.Context(),
		`UPDATE `+table.Qualified()+` SET label = repeat('x', $1) WHERE uniq = $2`,
		2*dataFaultLabelWidth, dataFaultViolator)
	require.NoError(t, err)
}

// dataFaultAlter narrows table's label to a width the planted row exceeds.
func dataFaultAlter(table testutil.WorkloadTable) string {
	return fmt.Sprintf("ALTER TABLE %s ALTER COLUMN label TYPE varchar(%d)", table.Qualified(), dataFaultLabelWidth)
}

// assertDataFaultVerdict holds the run's error and its stdout to the
// data-fault contract: an operational error that is not a refusal, and a
// failed verdict coded data-fault whose detail says the run was abandoned,
// names what was released, and offers no --restart, since nothing remains
// for one to discard.
func assertDataFaultVerdict(t *testing.T, runErr error, stdout string, table testutil.WorkloadTable, released string) {
	t.Helper()
	require.Error(t, runErr, "a data fault is an operational error")
	require.NotErrorIs(t, runErr, verdict.ErrRefused, "a data fault is not a refusal: the process exits 1, not 2")
	var abandoned *schemachange.AbandonedError
	require.ErrorAs(t, runErr, &abandoned, "the error is the Runner's abandonment")
	var pgErr *pgconn.PgError
	require.ErrorAs(t, runErr, &pgErr, "the abandonment carries the server's rejection")
	assert.Equal(t, stringTooLong, pgErr.Code, "the fault is the narrowed type refusing the planted row")

	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(stdout), &v), "the failed verdict is on stdout: %q", stdout)
	assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
	assert.Equal(t, string(executor.CodeDataFault), v.Code)
	assert.Equal(t, table.Schema+"."+table.Table, v.Table)
	assert.False(t, v.Forced)
	assert.Contains(t, v.Detail, "the run was abandoned")
	assert.Contains(t, v.Detail, released)
	assert.NotContains(t, v.Detail, "--restart", "nothing remains for a restart to discard")
}

// assertDataFaultLeftNothing holds the server to what an abandon promises:
// no shadow, no write fence on the source, no checkpoint row, and the
// source its original shape, its rows intact, and taking writes.
func assertDataFaultLeftNothing(t *testing.T, pool *pgxpool.Pool, table testutil.WorkloadTable) {
	t.Helper()
	var shadowExists bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1 AND c.relname = $2)`,
		table.Schema, schemachange.ShadowName(table.Schema, table.Table)).Scan(&shadowExists))
	assert.False(t, shadowExists, "the shadow is dropped")

	var fenced bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM pg_trigger tg
		   JOIN pg_class c ON c.oid = tg.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = $1 AND c.relname = $2 AND tg.tgname = $3)`,
		table.Schema, table.Table, schemachange.WriteFenceTrigger).Scan(&fenced))
	assert.False(t, fenced, "no write fence stands on the source")

	store, err := checkpoint.NewStore(pool, checkpoint.Options{})
	require.NoError(t, err)
	_, err = store.Inspect(t.Context(), table.Schema, table.Table)
	assert.ErrorIs(t, err, checkpoint.ErrNotFound, "the checkpoint row is deleted")

	var typ string
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT format_type(a.atttypid, a.atttypmod) FROM pg_attribute a
		   JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = $1 AND c.relname = $2 AND a.attname = 'label'`,
		table.Schema, table.Table).Scan(&typ))
	assert.Equal(t, "text", typ, "the source keeps its shape")

	var violators int
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT count(*) FROM `+table.Qualified()+` WHERE length(label) > $1`, dataFaultLabelWidth).Scan(&violators))
	assert.Equal(t, 1, violators, "the planted row is still the one the statement cannot hold")
	var rows int
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM `+table.Qualified()).Scan(&rows))
	assert.GreaterOrEqual(t, rows, dataFaultRows, "every seeded row is still in the source")

	_, err = pool.Exec(t.Context(),
		`INSERT INTO `+table.Qualified()+` (uniq, amount, label) VALUES ($1, 1.25, 'after-abandon')`, dataFaultRows*10)
	assert.NoError(t, err, "the source takes writes")
}
