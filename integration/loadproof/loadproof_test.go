// Package loadproof_test is the operator-run load proof of the copy-and-swap
// route: the built binary carries an identity-key retype over a large table
// while a concurrent workload writes it through the copy, the catch-up, the
// verify pass, the drain, and the swap, and the result is judged by oracles
// that make no timing assumption. The row oracle judges every row whose
// last write the load recorded below the WAL position the swap read under
// its lock, the drain's and the lock wait's writes included; it leaves out
// the rows written after the swap, which exist only in the live table, and
// the rows of writes whose client deadline fired before their outcome was
// heard, and results.json reports how many keys that leaves out. It is
// skipped unless LOAD_PROOF=1; `make load-proof` builds the binary and runs
// it against the compose database, and the same variables point it at any
// other server with logical decoding on (see docs/testing.md).
package loadproof_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/verdict"
)

// config is what the operator sets through the environment.
type config struct {
	// DSN is the database the load writes and the binary changes.
	DSN string
	// Binary is the pg-sprite executable under proof.
	Binary string
	// Rows seeds the table before the load starts.
	Rows int
	// Workers and RatePerSecond size the load: Workers × RatePerSecond
	// mutations a second in aggregate.
	Workers       int
	RatePerSecond int
	// HotRowFraction is the share of the load's updates aimed at the
	// table's ten lowest keys.
	HotRowFraction float64
	// FreeDisk is the figure handed to --free-disk: the operator's
	// statement of the free space on the volume the table lives on. It has
	// no default, because the preflight's disk-headroom refusal is only as
	// good as the figure it is given.
	FreeDisk string
	// AllowRemote lets DSN name a host that is not this machine; the
	// operator sets it to say that server is disposable.
	AllowRemote bool
	// Results is where the run's results.json is written.
	Results string
}

// Defaults for the variables an operator leaves unset. The row count is
// the production-like size the proof is meant at; a smoke run sets
// LOAD_PROOF_ROWS lower. The default load spreads its updates over the
// whole table: a chunk holding a key that is written faster than the live
// pass can reread it never settles, and the run stops with the chunk
// unsettled and the advice to run again when the table is quieter.
// LOAD_PROOF_HOT aims that share of the updates at the ten lowest keys to
// put the run under exactly that load.
const (
	defaultRows           = 1_000_000
	defaultWorkers        = 8
	defaultRatePerSecond  = 100
	defaultHotRowFraction = 0
	defaultResults        = "results.json"
)

// readConfig skips the test unless the operator asked for it, then reads
// the rest of the configuration, failing on a value it cannot use.
func readConfig(t *testing.T) config {
	t.Helper()
	if os.Getenv("LOAD_PROOF") != "1" {
		t.Skip("set LOAD_PROOF=1 to run the load proof; `make load-proof` sets everything")
	}
	c := config{
		DSN:            os.Getenv("LOAD_PROOF_DSN"),
		Binary:         os.Getenv("LOAD_PROOF_BIN"),
		Rows:           envInt(t, "LOAD_PROOF_ROWS", defaultRows),
		Workers:        envInt(t, "LOAD_PROOF_WORKERS", defaultWorkers),
		RatePerSecond:  envInt(t, "LOAD_PROOF_RATE", defaultRatePerSecond),
		HotRowFraction: envFloat(t, "LOAD_PROOF_HOT", defaultHotRowFraction),
		FreeDisk:       os.Getenv("LOAD_PROOF_FREE_DISK"),
		AllowRemote:    os.Getenv("LOAD_PROOF_ALLOW_REMOTE") == "1",
		Results:        envOr("LOAD_PROOF_RESULTS", defaultResults),
	}
	require.NotEmpty(t, c.DSN, "set LOAD_PROOF_DSN to a database with wal_level=logical")
	require.NotEmpty(t, c.Binary, "set LOAD_PROOF_BIN to a freshly built pg-sprite binary")
	require.Positive(t, c.Rows, "LOAD_PROOF_ROWS")
	require.NotEmpty(t, c.FreeDisk, "set LOAD_PROOF_FREE_DISK to the free space on the database's data volume (for example 40GiB); the copy builds a second full table beside the source, and the preflight refuses the run when the figure cannot hold it")
	target, err := pgconn.ParseConfig(c.DSN)
	require.NoError(t, err, "LOAD_PROOF_DSN")
	if !c.AllowRemote {
		require.True(t, targetIsLocal(target), "LOAD_PROOF_DSN names a host other than this machine, and the proof seeds and copies a table of LOAD_PROOF_ROWS rows on it; set LOAD_PROOF_ALLOW_REMOTE=1 when that server is disposable")
	}
	t.Logf("load proof target: host %s, port %d, database %s, user %s", target.Host, target.Port, target.Database, target.User)
	return c
}

// requireTimeForTheRun fails the test at its start when go test would end
// it before runBudget: a test the go test deadline ends is killed without
// its cleanups, which leaves the binary, its replication slot, and the
// seeded table behind. `make load-proof` passes a deadline that fits.
func requireTimeForTheRun(t *testing.T) {
	t.Helper()
	deadline, ok := t.Deadline()
	if !ok {
		return
	}
	require.Greater(t, time.Until(deadline), runBudget, "go test would stop the proof before the binary's own budget runs out and skip its cleanup; run it with -timeout above %s", runBudget)
}

func envFloat(t *testing.T, name string, fallback float64) float64 {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(v, 64)
	require.NoError(t, err, "%s must be a number", name)
	return f
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func envInt(t *testing.T, name string, fallback int) int {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	require.NoError(t, err, "%s must be an integer", name)
	return n
}

// Polling deadlines. None of them bounds the run under proof; each bounds
// a wait for the load to show one more commit, which a healthy database
// delivers within a tick or two.
const (
	// firstCommitDeadline bounds the wait for the load's first commit
	// before the binary starts, so the run begins over a table that is
	// being written.
	firstCommitDeadline = 30 * time.Second
	// afterSwapInsertDeadline bounds the wait for an insert after the
	// binary exits, proof that the application kept writing the table
	// through the swap and that the identity sequence handed to the
	// swapped-in table is past every key it holds: a sequence left behind
	// makes every insert collide on the primary key, which the load counts
	// as a race and never commits.
	afterSwapInsertDeadline = 30 * time.Second
	// commitPollInterval paces both waits.
	commitPollInterval = 50 * time.Millisecond
	// runBudget bounds the binary itself; a run that outlives it is a
	// hang, not a slow copy.
	runBudget = 2 * time.Hour
)

// loadMix is the workload the proof carries: inserts, updates at random
// and on the hot set when LOAD_PROOF_HOT asks for one, and deletes, with
// half the updates rewriting the
// out-of-line column — each a change the stream and the drain carry
// differently. Unique-key swaps and key moves are left out: a move that
// straddles a chunk the copier is reading collides on the copier's own
// insert with the shadow row the flush already landed, so a load that
// moves keys cannot run through the copy.
var loadMix = testutil.Mix{Insert: 2, Update: 3, Delete: 1}

// alterTemplate is the change under proof, over the workload table's
// qualified name: the identity primary key itself widens, so every chunk
// the copier reads and every change the stream carries is under the
// retyped column. The table starts as
//
//	CREATE TABLE workload (
//	    id integer GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
//	    uniq integer NOT NULL UNIQUE,
//	    amount numeric(12,2) NOT NULL,
//	    label text NOT NULL,
//	    blob text,               -- STORAGE EXTERNAL, always out of line
//	    updated_at timestamptz NOT NULL DEFAULT now()
//	);
const alterTemplate = `ALTER TABLE %s ALTER COLUMN id TYPE bigint`

// The identity primary key widens from integer to bigint under load. Every
// chunk the copier reads and every change the stream carries is under the
// retyped column; after the swap the live table holds the source's rows
// under the new type and the load keeps writing it.
func TestLoadProof(t *testing.T) {
	c := readConfig(t)
	requireTimeForTheRun(t)
	ctx := t.Context()
	res := newResults(c)
	t.Cleanup(func() {
		res.Passed = !t.Failed()
		if err := res.write(c.Results); err != nil {
			t.Errorf("write results: %v", err)
		}
		t.Logf("results written to %s", c.Results)
	})

	pool := newPool(t, c)
	require.NoError(t, pool.QueryRow(ctx, `SHOW server_version`).Scan(&res.ServerVersion))
	t.Logf("load proof server version %s", res.ServerVersion)
	res.Binary.SHA256 = fileSHA256(t, c.Binary)

	shape := testutil.WorkloadShape{IDType: "integer"}
	table := testutil.NewWorkloadTableOf(t, pool, shape)
	removeLeftoversOnCleanup(t, pool, table)
	t.Logf("seeding %d rows into %s", c.Rows, table.Qualified())
	require.NoError(t, table.SeedRows(ctx, c.Rows))

	load := testutil.StartLoad(t, pool, table, testutil.LoadSpec{
		Seed:                 1,
		Workers:              c.Workers,
		RatePerSecond:        c.RatePerSecond,
		Mix:                  loadMix,
		HotRowFraction:       c.HotRowFraction,
		ToastRewriteFraction: 0.5,
		RecordFailures:       true,
	})
	waitForLoad(t, load, func(p testutil.Progress) bool { return p.Committed() > 0 },
		firstCommitDeadline, "the load's first commit before the run starts")

	alter := fmt.Sprintf(alterTemplate, table.Qualified())
	watch := startWatch(t, pool, table, load)
	atStart := load.Progress()
	started := time.Now()
	res.StartedAt = started
	output, diagnostic, code := runBinary(t, c, "migrate",
		"--copy-and-swap",
		"--keep-old",
		"--free-disk", c.FreeDisk,
		"--json",
		"--alter", alter)
	exited := time.Now()
	atExit := load.Progress()
	res.RunMillis = exited.Sub(started).Milliseconds()
	w := watch.stop()
	res.recordWatch(w, started)
	res.recordOutput(output)
	// The load is recorded as it stands at exit before any judgement, so a
	// failed run's record still says what the table was under; the final
	// summary replaces it once the load has stopped.
	res.recordLoad(load.Snapshot(), atExit)

	require.Equal(t, 0, code, "binary stdout: %s; stderr: %s", output, diagnostic)
	var v verdict.Verdict
	require.NoError(t, json.Unmarshal(output, &v), "verdict JSON: %s", output)
	require.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome, "%s: %s", v.Reason, v.Detail)
	require.NotEmpty(t, v.SwapLSN, "a run that committed its own swap reports the swap's WAL position")
	swap, err := decode.ParseLSN(v.SwapLSN)
	require.NoError(t, err)
	require.NoError(t, w.Err, "the checkpoint poll failed")
	require.True(t, w.MarkSet, "the poll never saw the row before the cutover, so no write is known to have committed before the swap")
	requireMarkSpansTheStream(t, w, atStart)

	waitForLoad(t, load, func(p testutil.Progress) bool { return p.Inserts > atExit.Inserts },
		afterSwapInsertDeadline, "an insert after the swap")
	summary, err := load.Stop()
	require.NoError(t, err)
	res.recordLoad(summary, atExit)

	assertRowsConverged(t, pool, table, summary, swap, res)
	assertCatalogMatchesNative(t, pool, table, shape)
	window := swapWindow{opened: w.MarkAt, closed: exited}
	for _, f := range summary.Failures {
		assert.True(t, failureAllowed(f, window), "load failure the swap is not allowed to cause, at %s (mark %s, exit %s): SQLSTATE %q: %v",
			f.At.Format(time.RFC3339Nano), w.MarkAt.Format(time.RFC3339Nano), exited.Format(time.RFC3339Nano), f.Code, f.Err)
	}
}

// requireMarkSpansTheStream holds the mark to what the row oracle needs to
// judge the stream rather than only the copy: the last poll before the
// cutover saw the verify pass, which follows the copy and the catch-up, and
// between the binary's start and the mark the load committed an insert, an
// update, and a delete, each of which the oracle then compares.
func requireMarkSpansTheStream(t *testing.T, w watched, atStart testutil.Progress) {
	t.Helper()
	require.Equal(t, checkpoint.PhaseVerifying, w.MarkPhase, "the last poll before the swap saw %s, not the verify pass, so the judged writes may end before the stream carried any", w.MarkPhase)
	require.Greater(t, w.Mark.Inserts, atStart.Inserts, "the load inserted between the binary's start and the mark")
	require.Greater(t, w.Mark.Updates, atStart.Updates, "the load updated between the binary's start and the mark")
	require.Greater(t, w.Mark.Deletes, atStart.Deletes, "the load deleted between the binary's start and the mark")
}

// harnessConns is how many connections the harness's own reads — the
// checkpoint poll, the slot lag read, and the oracles — need beside the
// load's workers.
const harnessConns = 4

// newPool opens the application's connections: a plain pgx pool, as an
// application would hold, with room for every worker and the harness's
// own reads beside them.
func newPool(t *testing.T, c config) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(c.DSN)
	require.NoError(t, err)
	cfg.MaxConns = int32(c.Workers) + harnessConns
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, pool.Ping(t.Context()))
	return pool
}

// runBinary runs the binary under proof with args and returns its stdout,
// stderr, and exit code; a binary that cannot be started fails the test.
// The connection string reaches the binary through PGSPRITE_URL rather than
// --url, so its password is not on the process's command line.
func runBinary(t *testing.T, c config, args ...string) (output []byte, diagnostic string, code int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), runBudget)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Binary, args...)
	cmd.Env = append(os.Environ(), "PGSPRITE_URL="+c.DSN)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		require.True(t, errors.As(err, &exit), "the binary could not run: %v", err)
		code = exit.ExitCode()
	}
	return output, stderr.String(), code
}

// waitForLoad waits until the load's progress satisfies reached, failing
// the test with what after the deadline.
func waitForLoad(t *testing.T, load *testutil.LoadGenerator, reached func(testutil.Progress) bool, deadline time.Duration, what string) {
	t.Helper()
	require.Eventually(t, func() bool { return reached(load.Progress()) },
		deadline, commitPollInterval, "waiting for %s; load so far %+v", what, load.Progress())
}

// assertRowsConverged holds the retained source and the live table to the
// row oracle over every row whose last write the load recorded below the
// swap's WAL position (see excludedKeys): such a write committed before the
// swap took its lock, so it is in both relations, while a later write, or
// one whose outcome the load never heard, is taken out of the comparison
// on both sides rather than judged.
func assertRowsConverged(t *testing.T, pool *pgxpool.Pool, table testutil.WorkloadTable, summary testutil.Summary, swap decode.LSN, res *results) {
	t.Helper()
	report, excluded, err := judgeRows(t.Context(), pool, table, summary, swap)
	require.NoError(t, err)
	res.Rows.Compared = report.SourceCount
	res.Rows.Excluded = len(excluded)
	res.Rows.Ambiguous = len(summary.Ambiguous)
	assert.True(t, report.Converged(), "the retained source and the live table differ: %+v", report.Differences)
	assert.Positive(t, report.SourceCount, "the comparison covered no rows")
}

// assertCatalogMatchesNative holds the live table to the catalog oracle: its
// record equals that of a fresh table of the same shape that the statement
// changed natively.
func assertCatalogMatchesNative(t *testing.T, pool *pgxpool.Pool, table testutil.WorkloadTable, shape testutil.WorkloadShape) {
	t.Helper()
	reference := testutil.NewWorkloadTableOf(t, pool, shape)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(alterTemplate, reference.Qualified()))
	require.NoError(t, err)
	want, err := testutil.ReadTableCatalog(t.Context(), pool, reference.Schema, reference.Table)
	require.NoError(t, err)
	got, err := testutil.ReadTableCatalog(t.Context(), pool, table.Schema, table.Table)
	require.NoError(t, err)
	assert.Equal(t, want, got, "the swapped-in table differs from one the statement changed natively")
}
