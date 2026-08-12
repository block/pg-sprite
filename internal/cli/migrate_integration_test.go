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
	"github.com/block/pg-sprite/pkg/preflight"
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
		Alter:             alter,
		MaxTableSize:      1 << 30,
		IndexBuildTimeout: time.Minute,
		ValidateTimeout:   time.Minute,
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

// Acceptance (ii): a rewrite-requiring change is refused up front — the
// planner classifies the type change as copy-and-swap before anything runs,
// so no attempt ever holds ACCESS EXCLUSIVE doing rewrite work.
func TestMigrateRefusesTypeRewriteAsUnavailable(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, v text)", schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(
		"INSERT INTO %s.t SELECT g, repeat('x', 100) FROM generate_series(1, 1000) g", schema))
	require.NoError(t, err)

	cmd := newMigrateCmd(url, fmt.Sprintf("ALTER TABLE %s.t ALTER COLUMN id TYPE bigint", schema))
	cmd.JSON = true
	var out strings.Builder
	err = cmd.run(t.Context(), &out)
	require.ErrorIs(t, err, verdict.ErrRefused)

	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonBackendUnavailable, v.Reason)
	assert.Equal(t, schema+".t", v.Table)

	var typ string
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT data_type FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = 't' AND column_name = 'id'`, schema).Scan(&typ))
	assert.Equal(t, "integer", typ, "the refused change must not touch the schema")
	var count int
	require.NoError(t, pool.QueryRow(t.Context(),
		fmt.Sprintf("SELECT count(*) FROM %s.t", schema)).Scan(&count))
	assert.Equal(t, 1000, count, "the refused change must not touch the data")
}

// A blind attempt that cannot get its lock is cancelled by the lock budget
// and refused with the typed cause — the Phase 1 refusal contract, now
// reached through the routed execute path.
func TestMigrateRefusesOnLockBudgetContention(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", schema))
	require.NoError(t, err)

	// An open transaction holding ACCESS SHARE on the table makes the
	// ALTER's ACCESS EXCLUSIVE unobtainable until rollback.
	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() {
		if err := tx.Rollback(context.WithoutCancel(t.Context())); err != nil {
			t.Logf("rollback lock-holding transaction: %v", err)
		}
	}()
	_, err = tx.Exec(t.Context(), fmt.Sprintf("SELECT count(*) FROM %s.t", schema))
	require.NoError(t, err)

	cmd := newMigrateCmd(url, fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN age int", schema))
	cmd.LockTimeout = 100 * time.Millisecond
	cmd.JSON = true
	var out strings.Builder
	err = cmd.run(t.Context(), &out)
	require.ErrorIs(t, err, verdict.ErrRefused)

	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonBudgetExceeded, v.Reason)
	assert.Equal(t, verdict.CauseLockBudget, v.Cause)
}

// The engine substitutes the planner's safer sequence by default: a direct
// SET NOT NULL runs as the NOT VALID + VALIDATE + SET NOT NULL + DROP
// scaffold sequence, the verdict reports what actually ran, and no scaffold
// constraint is left behind.
func TestMigrateSubstitutesSetNotNullSequence(t *testing.T) {
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

	cmd := newMigrateCmd(url, fmt.Sprintf("ALTER TABLE %s.t ALTER COLUMN v SET NOT NULL", schema))
	cmd.JSON = true
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))

	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	assert.Len(t, v.ExecutedSQL, 4, "the four-step SET NOT NULL sequence must be reported")

	var notNull bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT attnotnull FROM pg_attribute
		 WHERE attrelid = ($1 || '.t')::regclass AND attname = 'v'`, schema).Scan(&notNull))
	assert.True(t, notNull, "the column must be NOT NULL")
	var scaffolds int
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT count(*) FROM pg_constraint con
		   JOIN pg_class c ON c.oid = con.conrelid
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = $1 AND c.relname = 't' AND con.contype = 'c'`, schema).Scan(&scaffolds))
	assert.Zero(t, scaffolds, "the scaffold CHECK constraint must be dropped")
}

// A blocking CREATE INDEX is substituted with its concurrent build and
// driven to a verified valid index.
func TestMigrateSubstitutesBlockingCreateIndex(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, c int)", schema))
	require.NoError(t, err)

	cmd := newMigrateCmd(url, fmt.Sprintf("CREATE INDEX t_c_idx ON %s.t (c)", schema))
	cmd.JSON = true
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))

	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	require.Len(t, v.ExecutedSQL, 1, "the substituted concurrent build must be reported")

	var valid bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT i.indisvalid FROM pg_index i
		 WHERE i.indexrelid = ($1 || '.t_c_idx')::regclass`, schema).Scan(&valid))
	assert.True(t, valid, "the index must be built and valid")
}

// A submitted CREATE INDEX CONCURRENTLY is already the online idiom: the
// engine drives it directly, with no substitution to report.
func TestMigrateRunsSubmittedConcurrentIndexBuild(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, c int)", schema))
	require.NoError(t, err)

	cmd := newMigrateCmd(url, fmt.Sprintf("CREATE INDEX CONCURRENTLY t_cic_idx ON %s.t (c)", schema))
	cmd.JSON = true
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))

	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	assert.Empty(t, v.ExecutedSQL, "no substitution happened, so none is reported")

	var valid bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT i.indisvalid FROM pg_index i
		 WHERE i.indexrelid = ($1 || '.t_cic_idx')::regclass`, schema).Scan(&valid))
	assert.True(t, valid, "the index must be built and valid")
}

// A safer-idiom decision without a constructible rewrite (an inline
// constraint on ADD COLUMN) is refused as rewrite-required — running the
// submitted form would falsify the plan's own reason.
func TestMigrateRefusesInlineConstraintAsRewriteRequired(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", schema))
	require.NoError(t, err)

	cmd := newMigrateCmd(url, fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN e int UNIQUE", schema))
	cmd.JSON = true
	var out strings.Builder
	err = cmd.run(t.Context(), &out)
	require.ErrorIs(t, err, verdict.ErrRefused)

	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonRewriteRequired, v.Reason)

	var n int
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = 't' AND column_name = 'e'`, schema).Scan(&n))
	assert.Zero(t, n, "the refused change must not execute")
}

// R3: an unqualified statement is resolved once against the session's
// search_path and re-emitted qualified, so the substituted concurrent index
// build — which the executor refuses to run against an unqualified name —
// succeeds, and the verdict names the resolved relation.
func TestMigrateResolvesUnqualifiedTableViaSearchPath(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, c int)", schema))
	require.NoError(t, err)

	// The command's sessions resolve the bare name through search_path.
	u, err := neturl.Parse(url)
	require.NoError(t, err)
	q := u.Query()
	q.Set("options", "-csearch_path="+schema)
	u.RawQuery = q.Encode()
	cmd := newMigrateCmd(u.String(), "CREATE INDEX t_c_idx ON t (c)")
	cmd.JSON = true
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))

	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	assert.Equal(t, schema+".t", v.Table, "the verdict must carry the resolved qualified name")
	require.Len(t, v.ExecutedSQL, 1)
	assert.Contains(t, v.ExecutedSQL[0], schema+".t", "the executed SQL must be schema-qualified")

	var valid bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT i.indisvalid FROM pg_index i
		 WHERE i.indexrelid = ($1 || '.t_c_idx')::regclass`, schema).Scan(&valid))
	assert.True(t, valid, "the index must be built and valid")
}

// An unqualified name that resolves nowhere on the session search_path is an
// operational error before anything is classified or executed.
func TestMigrateUnresolvableTableIsAnError(t *testing.T) {
	url := testutil.StartPostgres(t)
	cmd := newMigrateCmd(url, "ALTER TABLE nowhere_to_be_found ADD COLUMN c int")
	var out strings.Builder
	err := cmd.run(t.Context(), &out)
	require.ErrorIs(t, err, preflight.ErrTableNotFound)
	assert.Empty(t, out.String(), "no verdict is printed for an operational error")
}

// --force with the exact qualified-table acknowledgement runs the submitted
// form as-is: no substitution happens, the verdict records the override, and
// the change commits.
func TestMigrateForceRunsSubmittedFormOverSubstitution(t *testing.T) {
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

	cmd := newMigrateCmd(url, fmt.Sprintf("ALTER TABLE %s.t ALTER COLUMN v SET NOT NULL", schema))
	cmd.Force = schema + ".t"
	cmd.JSON = true
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))

	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	assert.True(t, v.Forced, "the verdict must record the override")
	assert.Empty(t, v.ExecutedSQL, "the submitted form ran as-is; no substitution to report")

	var notNull bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT attnotnull FROM pg_attribute
		 WHERE attrelid = ($1 || '.t')::regclass AND attname = 'v'`, schema).Scan(&notNull))
	assert.True(t, notNull, "the forced change must have committed")
}

// --force also overrides a backend-unavailable refusal: the rewrite-carrying
// type change runs as a blind bounded attempt and commits on a small table.
func TestMigrateForceRunsUnavailableRewrite(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", schema))
	require.NoError(t, err)

	cmd := newMigrateCmd(url, fmt.Sprintf("ALTER TABLE %s.t ALTER COLUMN id TYPE bigint", schema))
	cmd.Force = schema + ".t"
	cmd.JSON = true
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))

	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	assert.True(t, v.Forced)

	var typ string
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT data_type FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = 't' AND column_name = 'id'`, schema).Scan(&typ))
	assert.Equal(t, "bigint", typ, "the forced rewrite must have committed")
}

// A --force acknowledgement that does not name the resolved target table is
// a usage error: nothing executes.
func TestMigrateForceAckMismatchExecutesNothing(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, v text)", schema))
	require.NoError(t, err)

	cmd := newMigrateCmd(url, fmt.Sprintf("ALTER TABLE %s.t ALTER COLUMN v SET NOT NULL", schema))
	cmd.Force = "wrong.table"
	var out strings.Builder
	err = cmd.run(t.Context(), &out)
	require.Error(t, err)
	require.NotErrorIs(t, err, verdict.ErrRefused, "an acknowledgement mismatch is a usage error, not a refusal")
	assert.Empty(t, out.String(), "no verdict is printed")

	var notNull bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT attnotnull FROM pg_attribute
		 WHERE attrelid = ($1 || '.t')::regclass AND attname = 'v'`, schema).Scan(&notNull))
	assert.False(t, notNull, "nothing must have executed")
}

// --force overrides routing only, never the executor's protections: the
// forced blind attempt is still size-guarded.
func TestMigrateForceKeepsSizeGuard(t *testing.T) {
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

	cmd := newMigrateCmd(url, fmt.Sprintf("ALTER TABLE %s.t ALTER COLUMN id TYPE bigint", schema))
	cmd.Force = schema + ".t"
	cmd.MaxTableSize = 1
	cmd.JSON = true
	var out strings.Builder
	err = cmd.run(t.Context(), &out)
	require.ErrorIs(t, err, verdict.ErrRefused)

	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.ReasonTableTooLarge, v.Reason)
	assert.True(t, v.Forced, "a refused forced attempt must still record the override machine-readably")

	var typ string
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT data_type FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = 't' AND column_name = 'id'`, schema).Scan(&typ))
	assert.Equal(t, "integer", typ, "the guarded change must not have executed")
}

// The flagship force case from the design docs: a blocking CREATE INDEX,
// forced past its concurrent-build substitution, runs as-is as a blind
// bounded attempt and commits.
func TestMigrateForceRunsBlockingCreateIndex(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, c int)", schema))
	require.NoError(t, err)

	cmd := newMigrateCmd(url, fmt.Sprintf("CREATE INDEX t_forced_idx ON %s.t (c)", schema))
	cmd.Force = schema + ".t"
	cmd.JSON = true
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))

	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	assert.True(t, v.Forced, "the verdict must record the override")
	assert.Empty(t, v.ExecutedSQL, "the submitted form ran as-is; no substitution to report")

	var valid bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT i.indisvalid FROM pg_index i
		 WHERE i.indexrelid = ($1 || '.t_forced_idx')::regclass`, schema).Scan(&valid))
	assert.True(t, valid, "the forced blocking build must have committed a valid index")
}

// A forced blind attempt that runs past its statement budget is cancelled
// and refused with the typed cause — the statement-budget refusal contract
// end to end — and the refusal still records the override.
func TestMigrateForcedRewriteRefusedByStatementBudget(t *testing.T) {
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
	cmd.Force = schema + ".t"
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
	assert.True(t, v.Forced, "a refused forced attempt must still record the override machine-readably")

	var typ string
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT data_type FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = 't' AND column_name = 'id'`, schema).Scan(&typ))
	assert.Equal(t, "integer", typ, "the cancelled attempt must not change the schema")
}

// A planner refusal (no known safe path) is not forceable: --force with a
// valid acknowledgement still ends in the refusal verdict and nothing
// executes.
func TestMigrateForceIgnoredForRefusedRoute(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, room int)", schema))
	require.NoError(t, err)

	cmd := newMigrateCmd(url, fmt.Sprintf(
		"ALTER TABLE %s.t ADD CONSTRAINT ex EXCLUDE USING gist (room WITH =)", schema))
	cmd.Force = schema + ".t"
	cmd.JSON = true
	var out strings.Builder
	err = cmd.run(t.Context(), &out)
	require.ErrorIs(t, err, verdict.ErrRefused)

	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)
	assert.False(t, v.Forced, "nothing ran, and no override was honored")

	var n int
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT count(*) FROM pg_constraint con
		   JOIN pg_class c ON c.oid = con.conrelid
		   JOIN pg_namespace ns ON ns.oid = c.relnamespace
		  WHERE ns.nspname = $1 AND c.relname = 't' AND con.conname = 'ex'`, schema).Scan(&n))
	assert.Zero(t, n, "the refused change must not execute")
}

// Statements the gate admits but the executor's static admission refuses end
// in a typed refusal verdict, not a raw operational error — the "exactly one
// verdict" contract holds for every gate-admitted statement, and nothing
// executes.
func TestMigrateRefusesExecutorAdmissionStatically(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	t.Run("unnamed index build", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, c int)", schema))
		require.NoError(t, err)

		cmd := newMigrateCmd(url, fmt.Sprintf("CREATE INDEX ON %s.t (c)", schema))
		cmd.JSON = true
		var out strings.Builder
		err = cmd.run(t.Context(), &out)
		require.ErrorIs(t, err, verdict.ErrRefused)

		var v verdict.Verdict
		require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
		assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
		assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)

		var n int
		require.NoError(t, pool.QueryRow(t.Context(),
			`SELECT count(*) FROM pg_indexes WHERE schemaname = $1 AND tablename = 't' AND indexname <> 't_pkey'`,
			schema).Scan(&n))
		assert.Zero(t, n, "the refused build must not create an index")
	})

	t.Run("if not exists index build", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, c int)", schema))
		require.NoError(t, err)

		cmd := newMigrateCmd(url, fmt.Sprintf("CREATE INDEX IF NOT EXISTS t_c_idx ON %s.t (c)", schema))
		cmd.JSON = true
		var out strings.Builder
		err = cmd.run(t.Context(), &out)
		require.ErrorIs(t, err, verdict.ErrRefused)

		var v verdict.Verdict
		require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
		assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
		assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)

		var n int
		require.NoError(t, pool.QueryRow(t.Context(),
			`SELECT count(*) FROM pg_indexes WHERE schemaname = $1 AND indexname = 't_c_idx'`,
			schema).Scan(&n))
		assert.Zero(t, n, "the refused build must not create an index")
	})

	t.Run("detach partition", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf(
			"CREATE TABLE %s.p (id int PRIMARY KEY) PARTITION BY RANGE (id)", schema))
		require.NoError(t, err)
		_, err = pool.Exec(t.Context(), fmt.Sprintf(
			"CREATE TABLE %s.p1 PARTITION OF %s.p FOR VALUES FROM (0) TO (100)", schema, schema))
		require.NoError(t, err)

		cmd := newMigrateCmd(url, fmt.Sprintf("ALTER TABLE %s.p DETACH PARTITION p1", schema))
		cmd.JSON = true
		var out strings.Builder
		err = cmd.run(t.Context(), &out)
		require.ErrorIs(t, err, verdict.ErrRefused)

		var v verdict.Verdict
		require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
		assert.Equal(t, verdict.OutcomeRefused, v.Outcome)

		var attached bool
		require.NoError(t, pool.QueryRow(t.Context(),
			`SELECT EXISTS (SELECT 1 FROM pg_inherits
			  WHERE inhrelid = ($1 || '.p1')::regclass AND inhparent = ($1 || '.p')::regclass)`,
			schema).Scan(&attached))
		assert.True(t, attached, "the refused detach must leave the partition attached")
	})
}

// The size guard protects blind attempts only: a substituted safer sequence
// runs on a table above the threshold, because its long steps are online by
// design and its brief steps are budget-bounded.
func TestMigrateSizeGuardSkippedForSubstitutedSequence(t *testing.T) {
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

	cmd := newMigrateCmd(url, fmt.Sprintf("ALTER TABLE %s.t ALTER COLUMN v SET NOT NULL", schema))
	cmd.MaxTableSize = 1 // would refuse a blind attempt; must not gate the sequence
	cmd.JSON = true
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))

	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	assert.NotEmpty(t, v.ExecutedSQL)
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

// Acceptance (iv): non-ALTER TABLE statements are refused with the safe-idiom
// pointer and never executed. The gate needs no database at all.
func TestMigrateGateRefusesWithoutDatabase(t *testing.T) {
	tests := []struct {
		name       string
		alter      string
		reason     verdict.Reason
		saferIdiom string
	}{
		{"drop index", "DROP INDEX i", verdict.ReasonIndexStatement, "DROP INDEX CONCURRENTLY"},
		{"reindex", "REINDEX TABLE t", verdict.ReasonIndexStatement, "REINDEX ... CONCURRENTLY"},
		// The already-concurrent forms carry no safer idiom: suggesting the
		// statement the user submitted would loop a resubmitting automation.
		// CREATE INDEX (both forms) passes the gate: the blocking form is
		// substituted with its concurrent build, the concurrent form is
		// driven directly.
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
