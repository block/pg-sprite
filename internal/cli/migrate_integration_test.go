package cli

import (
	"context"
	"encoding/json"
	"fmt"
	neturl "net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/verdict"
)

// newMigrateCmd builds a MigrateCmd with the flag defaults kong would apply.
func newMigrateCmd(url, alter string) *MigrateCmd {
	return &MigrateCmd{
		DBFlags: DBFlags{
			URL:              url,
			LockTimeout:      3 * time.Second,
			StatementTimeout: 30 * time.Second,
		},
		Alter:        alter,
		MaxTableSize: 1 << 30,
	}
}

// Acceptance (i): an instant-eligible change runs and commits within budget.
func TestMigrateExecutesInstantChange(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", schema))
	require.NoError(t, err)

	cmd := newMigrateCmd(url, fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN age int", schema))
	cmd.JSON = true
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))

	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	assert.Equal(t, schema+".t", v.Table)

	var typ string
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT data_type FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = 't' AND column_name = 'age'`, schema).Scan(&typ))
	assert.Equal(t, "integer", typ)
}

// ALTER TABLE ... RENAME COLUMN parses as a RenameStmt, not an
// AlterTableStmt, but is a table-targeted instant catalog change: the front
// door must route it through, not refuse it as unsupported.
func TestMigrateExecutesRenameColumn(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, a int)", schema))
	require.NoError(t, err)

	cmd := newMigrateCmd(url, fmt.Sprintf("ALTER TABLE %s.t RENAME COLUMN a TO b", schema))
	cmd.JSON = true
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))

	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)

	var n int
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = 't' AND column_name = 'b'`, schema).Scan(&n))
	assert.Equal(t, 1, n, "the rename must have committed")
}

// Acceptance (ii): a rewrite-requiring change is cancelled, leaves schema and
// data unchanged, and returns the not-native-safe verdict with its reason.
func TestMigrateRefusesRewriteWithBudgetVerdict(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, v text)", schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(
		"INSERT INTO %s.t SELECT g, repeat('x', 100) FROM generate_series(1, 300000) g", schema))
	require.NoError(t, err)

	cmd := newMigrateCmd(url, fmt.Sprintf("ALTER TABLE %s.t ALTER COLUMN id TYPE bigint", schema))
	cmd.StatementTimeout = 50 * time.Millisecond
	cmd.JSON = true
	var out strings.Builder
	err = cmd.run(t.Context(), &out)
	require.ErrorIs(t, err, verdict.ErrRefused)

	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonBudgetExceeded, v.Reason)
	assert.Equal(t, verdict.CauseStatementBudget, v.Cause)
	assert.Equal(t, schema+".t", v.Table)

	var typ string
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT data_type FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = 't' AND column_name = 'id'`, schema).Scan(&typ))
	assert.Equal(t, "integer", typ, "the cancelled attempt must not change the schema")
	var count int
	require.NoError(t, pool.QueryRow(t.Context(),
		fmt.Sprintf("SELECT count(*) FROM %s.t", schema)).Scan(&count))
	assert.Equal(t, 300000, count, "the cancelled attempt must not change the data")
}

// Acceptance (iii): a table above the size threshold skips the attempt and
// returns the same verdict class.
func TestMigrateSizeGuardSkipsAttempt(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, v text)", schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(
		"INSERT INTO %s.t SELECT g, 'v' FROM generate_series(1, 1000) g", schema))
	require.NoError(t, err)

	cmd := newMigrateCmd(url, fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN age int", schema))
	cmd.MaxTableSize = 1 // guarantees the guard fires without a big fixture
	cmd.JSON = true
	var out strings.Builder
	err = cmd.run(t.Context(), &out)
	require.ErrorIs(t, err, verdict.ErrRefused)

	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.ReasonTableTooLarge, v.Reason)

	// The attempt was skipped, so the (instant-eligible) change must not
	// have been applied.
	var n int
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = 't' AND column_name = 'age'`, schema).Scan(&n))
	assert.Zero(t, n, "the size guard must skip the attempt entirely")
}

// A connected role without membership in the owning role is refused up
// front with a typed privilege verdict — instead of the server's mid-change
// "must be owner" error — and the same change executes once the exact
// membership the refusal names is granted.
func TestMigrateRefusesInsufficientPrivileges(t *testing.T) {
	serverURL := testutil.StartPostgres(t)
	admin, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: serverURL})
	require.NoError(t, err)
	defer admin.Close()

	owner := testutil.NewRole(t, admin, "NOLOGIN")
	const password = "engine-test-password"
	engine := testutil.NewRole(t, admin, "LOGIN PASSWORD '"+password+"'")
	schema := testutil.NewSchema(t, admin)
	_, err = admin.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", schema))
	require.NoError(t, err)
	_, err = admin.Exec(t.Context(), fmt.Sprintf("ALTER TABLE %s.t OWNER TO %s",
		schema, pgx.Identifier{owner}.Sanitize()))
	require.NoError(t, err)
	_, err = admin.Exec(t.Context(), fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s",
		schema, pgx.Identifier{engine}.Sanitize()))
	require.NoError(t, err)

	u, err := neturl.Parse(serverURL)
	require.NoError(t, err)
	u.User = neturl.UserPassword(engine, password)
	cmd := newMigrateCmd(u.String(), fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN age int", schema))
	cmd.JSON = true

	var out strings.Builder
	err = cmd.run(t.Context(), &out)
	require.ErrorIs(t, err, verdict.ErrRefused)
	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonInsufficientPrivileges, v.Reason)
	assert.Equal(t, schema+".t", v.Table)

	// The detail carries the exact provisioning statement, and the fix
	// below executes that same statement — the closed loop the verdict
	// promises an operator. The expected grant is version-dependent: 16+
	// membership grants carry the INHERIT option explicitly.
	grant := fmt.Sprintf("GRANT %s TO %s",
		pgx.Identifier{owner}.Sanitize(), pgx.Identifier{engine}.Sanitize())
	var versionNum int
	require.NoError(t, admin.QueryRow(t.Context(),
		"SELECT current_setting('server_version_num')::int").Scan(&versionNum))
	if versionNum >= 160000 {
		grant += " WITH INHERIT TRUE"
	}
	assert.Contains(t, v.Detail, grant, "the refusal detail must name the exact remediation")

	_, err = admin.Exec(t.Context(), grant)
	require.NoError(t, err)

	out.Reset()
	require.NoError(t, cmd.run(t.Context(), &out))
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
}

// Acceptance (iv): non-ALTER TABLE statements are refused with the safe-idiom
// pointer and never executed. The gate needs no database at all.
func TestMigrateGateRefusesWithoutDatabase(t *testing.T) {
	tests := []struct {
		name       string
		alter      string
		reason     verdict.Reason
		saferIdiom string
	}{
		{"create index", "CREATE INDEX i ON t (c)", verdict.ReasonIndexStatement, "CREATE INDEX CONCURRENTLY"},
		{"drop index", "DROP INDEX i", verdict.ReasonIndexStatement, "DROP INDEX CONCURRENTLY"},
		{"reindex", "REINDEX TABLE t", verdict.ReasonIndexStatement, "REINDEX ... CONCURRENTLY"},
		// The already-concurrent forms carry no safer idiom: suggesting the
		// statement the user submitted would loop a resubmitting automation.
		{"create index concurrently", "CREATE INDEX CONCURRENTLY i ON t (c)", verdict.ReasonIndexStatement, ""},
		{"drop index concurrently", "DROP INDEX CONCURRENTLY i", verdict.ReasonIndexStatement, ""},
		{"reindex concurrently", "REINDEX TABLE CONCURRENTLY t", verdict.ReasonIndexStatement, ""},
		{"alter index", "ALTER INDEX i SET (fillfactor = 90)", verdict.ReasonUnsupportedStatement, ""},
		{"create table", "CREATE TABLE t (id int)", verdict.ReasonUnsupportedStatement, "pg-sprite diff --desired schema.sql"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// An unroutable URL proves the gate refuses before connecting.
			cmd := newMigrateCmd("postgres://nobody@localhost:1/nope", tt.alter)
			cmd.JSON = true
			var out strings.Builder
			err := cmd.run(t.Context(), &out)
			require.ErrorIs(t, err, verdict.ErrRefused)

			var v verdict.Verdict
			require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
			assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
			assert.Equal(t, tt.reason, v.Reason)
			assert.Equal(t, tt.saferIdiom, v.SaferIdiom)
		})
	}
}

func TestMigrateSurfacesParseErrors(t *testing.T) {
	cmd := newMigrateCmd("postgres://nobody@localhost:1/nope", "ALTER TABEL t ADD COLUMN x int")
	var out strings.Builder
	err := cmd.run(t.Context(), &out)
	require.Error(t, err)
	assert.NotErrorIs(t, err, verdict.ErrRefused, "a parse failure is an operational error, not a refusal")
}

// syncWriter guards the diagnostics buffer: pgx tracelog can write from pool
// housekeeping goroutines concurrently with the command's own lifecycle logs.
type syncWriter struct {
	mu sync.Mutex
	b  strings.Builder
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// --debug emits lifecycle events and pgx statement tracing on the diagnostics
// stream, and never leaks them into the command's stdout output; without the
// flag diagnostics are discarded entirely.
func TestMigrateDebugDiagnostics(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", schema))
	require.NoError(t, err)

	t.Run("debug on", func(t *testing.T) {
		cmd := newMigrateCmd(url, fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN age int", schema))
		cmd.Debug = true
		cmd.JSON = true
		var diag syncWriter
		cmd.diagOut = &diag
		var out strings.Builder
		require.NoError(t, cmd.run(t.Context(), &out))

		// A strict decode of stdout proves diagnostics did not leak into the
		// command's output stream: any interleaved log line would break it.
		var v verdict.Verdict
		require.NoError(t, json.Unmarshal([]byte(out.String()), &v),
			"stdout must carry exactly the verdict, nothing else")
		assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)

		assert.NotEmpty(t, diag.String(),
			"--debug must emit diagnostics on the diagnostics stream")
	})

	t.Run("debug off discards diagnostics", func(t *testing.T) {
		cmd := newMigrateCmd(url, fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN age2 int", schema))
		var diag syncWriter
		cmd.diagOut = &diag
		var out strings.Builder
		require.NoError(t, cmd.run(t.Context(), &out))
		assert.Empty(t, diag.String())
	})
}

// Both output modes report the empty case: an empty JSON list, and a
// non-empty human explanation. The test gets a database of its own —
// status scopes to the connected database, and on a shared server other
// tests' pg-sprite sessions would otherwise show up here.
func TestStatusReportsNoSessions(t *testing.T) {
	url := testutil.NewDatabase(t, testutil.StartPostgres(t))

	cmd := &StatusCmd{DBFlags: DBFlags{URL: url}, JSON: true}
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))
	var sessions []session
	require.NoError(t, json.Unmarshal([]byte(out.String()), &sessions))
	assert.Empty(t, sessions)

	cmd = &StatusCmd{DBFlags: DBFlags{URL: url}}
	var text strings.Builder
	require.NoError(t, cmd.run(t.Context(), &text))
	assert.NotEmpty(t, text.String())
}

// pg_stat_activity nulls out state and query for other roles' backends when
// the viewer lacks pg_read_all_stats — the read-only-operator shape. status
// must render those sessions, not crash the scan.
func TestStatusHandlesOtherRolesSessions(t *testing.T) {
	superURL := testutil.StartPostgres(t)
	superPool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: superURL})
	require.NoError(t, err)
	t.Cleanup(superPool.Close)

	// Roles are cluster-global, so a unique name plus a cleanup drop keeps
	// a long-lived shared server reusable — even after a killed run that
	// never reached its cleanups.
	role := fmt.Sprintf("limited_%d", os.Getpid())
	_, err = superPool.Exec(t.Context(),
		"CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" LOGIN PASSWORD 'limited-test-only'")
	require.NoError(t, err)
	t.Cleanup(func() {
		// t.Context is cancelled by cleanup time; strip the cancellation.
		_, err := superPool.Exec(context.WithoutCancel(t.Context()),
			"DROP ROLE IF EXISTS "+pgx.Identifier{role}.Sanitize())
		if err != nil {
			t.Logf("drop role %s: %v", role, err)
		}
	})

	// Hold a superuser pg-sprite session open on a pinned connection so
	// pg_stat_activity is guaranteed to contain a foreign-role row while
	// status runs.
	conn, err := superPool.Acquire(t.Context())
	require.NoError(t, err)
	defer conn.Release()
	var pid int
	require.NoError(t, conn.QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&pid))

	u, err := neturl.Parse(superURL)
	require.NoError(t, err)
	u.User = neturl.UserPassword(role, "limited-test-only")

	cmd := &StatusCmd{DBFlags: DBFlags{URL: u.String()}, JSON: true}
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))

	var sessions []session
	require.NoError(t, json.Unmarshal([]byte(out.String()), &sessions))
	i := slices.IndexFunc(sessions, func(s session) bool { return s.PID == pid })
	require.GreaterOrEqual(t, i, 0, "the other role's session must be listed, not crash the scan")
}

// A pg-sprite session on another database of the same server belongs to
// another change: status scopes to the connected database.
func TestStatusScopesToConnectedDatabase(t *testing.T) {
	serverURL := testutil.StartPostgres(t)
	dbURL := testutil.NewDatabase(t, serverURL)

	// Hold a pg-sprite session open on the server's default database on a
	// pinned connection so pg_stat_activity is guaranteed to contain a
	// foreign-database row while status runs.
	otherPool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: serverURL})
	require.NoError(t, err)
	defer otherPool.Close()
	conn, err := otherPool.Acquire(t.Context())
	require.NoError(t, err)
	defer conn.Release()
	var pid int
	require.NoError(t, conn.QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&pid))

	cmd := &StatusCmd{DBFlags: DBFlags{URL: dbURL}, JSON: true}
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))

	var sessions []session
	require.NoError(t, json.Unmarshal([]byte(out.String()), &sessions))
	i := slices.IndexFunc(sessions, func(s session) bool { return s.PID == pid })
	assert.Equal(t, -1, i, "a session on another database must not be listed")
}

// A live pg-sprite session (any connection made through pkg/dbconn) is
// reported with its pid and per-session fields. The session is held open on a
// pinned connection so pg_stat_activity is guaranteed to contain it while
// status runs.
func TestStatusReportsActiveSession(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	conn, err := pool.Acquire(t.Context())
	require.NoError(t, err)
	defer conn.Release()
	var pid int
	// The marker alias makes the session's last-query text deterministic
	// for the field assertion below.
	require.NoError(t, conn.QueryRow(t.Context(),
		"SELECT pg_backend_pid() AS pgsprite_status_marker").Scan(&pid))

	cmd := &StatusCmd{DBFlags: DBFlags{URL: url}, JSON: true}
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))

	var sessions []session
	require.NoError(t, json.Unmarshal([]byte(out.String()), &sessions))
	i := slices.IndexFunc(sessions, func(s session) bool { return s.PID == pid })
	require.GreaterOrEqual(t, i, 0, "the held session must be listed")
	assert.Equal(t, "idle", sessions[i].State)
	assert.Contains(t, sessions[i].Query, "pgsprite_status_marker",
		"the session's last query must be reported")

	// The human rendering carries the same session.
	cmd = &StatusCmd{DBFlags: DBFlags{URL: url}}
	var text strings.Builder
	require.NoError(t, cmd.run(t.Context(), &text))
	assert.Contains(t, text.String(), strconv.Itoa(pid))
}
