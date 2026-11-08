// Package loadproof_test is the operator-run load proof of the copy-and-swap
// route: the built binary carries an identity-key retype over a large table
// while a concurrent workload writes it through the copy, the catch-up, the
// drain, and the swap, and the result is judged by oracles that make no
// timing assumption. It is skipped unless LOAD_PROOF=1; `make load-proof`
// builds the binary and runs it against the compose database, and the
// same variables point it at any other server with logical decoding on.
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

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/schemachange"
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
	// statement of the free space on the volume the table lives on.
	FreeDisk string
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
	defaultFreeDisk       = "64GiB"
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
		FreeDisk:       envOr("LOAD_PROOF_FREE_DISK", defaultFreeDisk),
		Results:        envOr("LOAD_PROOF_RESULTS", defaultResults),
	}
	require.NotEmpty(t, c.DSN, "set LOAD_PROOF_DSN to a database with wal_level=logical")
	require.NotEmpty(t, c.Binary, "set LOAD_PROOF_BIN to a freshly built pg-sprite binary")
	require.Positive(t, c.Rows, "LOAD_PROOF_ROWS")
	return c
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
	// afterSwapCommitDeadline bounds the wait for a commit after the
	// binary exits, proof that the application kept writing the table
	// through the swap.
	afterSwapCommitDeadline = 30 * time.Second
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

// allowedFailureCodes are the SQLSTATEs a load statement may fail with
// during the run: the swap holds the table's lock briefly and a statement
// that queues behind it past its lock_timeout fails with 55P03; a
// statement a connection had prepared before the swap fails once with
// 0A000 when its result type changed under it, and the driver re-prepares.
// Any other code is a failure the proof reports as its own.
var allowedFailureCodes = map[string]bool{
	"55P03": true,
	"0A000": true,
}

// failureAllowed reports whether a recorded load failure is one of the
// kinds the swap is allowed to cause. A statement that ran out its own
// client-side deadline while the swap held the lock is allowed on the
// same grounds as a lock timeout.
func failureAllowed(f testutil.Failure) bool {
	if errors.Is(f.Err, context.DeadlineExceeded) {
		return true
	}
	return allowedFailureCodes[f.Code]
}

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
	waitForCommits(t, load, 0, firstCommitDeadline, "the load's first commit before the run starts")

	alter := fmt.Sprintf(alterTemplate, table.Qualified())
	watch := startWatch(t, pool, table, load)
	started := time.Now()
	res.StartedAt = started
	output, diagnostic, code := runBinary(t, c, "migrate",
		"--url", c.DSN,
		"--copy-and-swap",
		"--keep-old",
		"--free-disk", c.FreeDisk,
		"--json",
		"--alter", alter)
	res.RunMillis = time.Since(started).Milliseconds()
	w := watch.stop()
	res.recordWatch(w, started)
	res.Verdict = json.RawMessage(bytes.TrimSpace(output))
	// The load is recorded as it stands at exit before any judgement, so a
	// failed run's record still says what the table was under; the final
	// summary replaces it once the load has stopped.
	committedAtExit := load.Snapshot().Committed()
	res.recordLoad(load.Snapshot(), committedAtExit)

	require.Equal(t, 0, code, "binary stdout: %s; stderr: %s", output, diagnostic)
	var v verdict.Verdict
	require.NoError(t, json.Unmarshal(output, &v), "verdict JSON: %s", output)
	require.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome, "%s: %s", v.Reason, v.Detail)
	require.NoError(t, w.Err, "the checkpoint poll failed")
	require.True(t, w.MarkSet, "the poll never saw the row before the cutover, so no write is known to have committed before the swap")

	waitForCommits(t, load, committedAtExit, afterSwapCommitDeadline, "a commit after the swap")
	summary, err := load.Stop()
	require.NoError(t, err)
	res.recordLoad(summary, committedAtExit)

	assertRowsConverged(t, pool, table, summary, w.Mark, res)
	assertCatalogMatchesNative(t, pool, table, shape)
	for _, f := range summary.Failures {
		assert.True(t, failureAllowed(f), "load failure the swap is not allowed to cause: SQLSTATE %q: %v", f.Code, f.Err)
	}
	require.Positive(t, summary.Inserts, "the load inserted during the run")
	require.Positive(t, summary.Updates, "the load updated during the run")
	require.Positive(t, summary.Deletes, "the load deleted during the run")
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
func runBinary(t *testing.T, c config, args ...string) (output []byte, diagnostic string, code int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), runBudget)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Binary, args...)
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

// waitForCommits waits until the load has committed more than seen,
// failing the test with what after the deadline.
func waitForCommits(t *testing.T, load *testutil.LoadGenerator, seen int, deadline time.Duration, what string) {
	t.Helper()
	require.Eventually(t, func() bool { return load.Snapshot().Committed() > seen },
		deadline, commitPollInterval, "waiting for %s; load so far %+v", what, load.Snapshot())
}

// assertRowsConverged holds the retained source and the live table to the
// row oracle over every row the load is not known to have touched after
// the mark: a write the poll saw committed while the row still said the
// swap had not happened is in both relations, and a later write may be in
// the live table alone, so those keys are taken out of the comparison on
// both sides rather than judged.
func assertRowsConverged(t *testing.T, pool *pgxpool.Pool, table testutil.WorkloadTable, summary testutil.Summary, mark int, res *results) {
	t.Helper()
	excluded := summary.TouchedIDs[mark:]
	report, err := testutil.Diff(t.Context(), pool,
		testutil.RelationRef{Schema: table.Schema, Table: schemachange.OldName(table.Schema, table.Table)},
		testutil.RelationRef{Schema: table.Schema, Table: table.Table},
		testutil.ConvergeOptions{ExcludeKeys: excluded})
	require.NoError(t, err)
	res.Rows.Compared = report.SourceCount
	res.Rows.Excluded = len(excluded)
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
