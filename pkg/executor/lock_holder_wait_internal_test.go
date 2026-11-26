// White-box tests of the wait a native attempt takes outside the lock queue:
// which holders it waits on, that it reads the real catalog on a session
// whose search_path lists a decoy schema ahead of pg_catalog, and that one
// deadline bounds the whole wait.

package executor

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/statement"
)

// holderWaitFixture is a server, an admin pool on it, and a throwaway schema.
type holderWaitFixture struct {
	url    string
	admin  *pgxpool.Pool
	schema string
}

func newHolderWaitFixture(t *testing.T) holderWaitFixture {
	t.Helper()
	url := testutil.StartPostgres(t)
	admin, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	return holderWaitFixture{url: url, admin: admin, schema: testutil.NewSchema(t, admin)}
}

func (f holderWaitFixture) exec(t *testing.T, sql string) {
	t.Helper()
	_, err := f.admin.Exec(t.Context(), fmt.Sprintf(sql, f.schema))
	require.NoError(t, err)
}

func parseStatement(t *testing.T, sql string) statement.Statement {
	t.Helper()
	st, err := statement.ParseOne(sql)
	require.NoError(t, err)
	return st
}

// holdLock begins a transaction on pool that runs sql, keeps it open until
// the test ends, and returns its backend PID.
func holdLock(t *testing.T, pool *pgxpool.Pool, sql string) int32 {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = tx.Rollback(context.WithoutCancel(t.Context()))
	})
	_, err = tx.Exec(t.Context(), sql)
	require.NoError(t, err)
	var pid int32
	require.NoError(t, tx.QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&pid))
	return pid
}

// requireHolderWaitError asserts err reports pid outlasting the wait.
func requireHolderWaitError(t *testing.T, err error, pid int32) {
	t.Helper()
	var holderErr *LockHolderWaitError
	require.ErrorAs(t, err, &holderErr)
	assert.Equal(t, pid, holderErr.PID)
	assert.Equal(t, CodeLockHolderWaitExceeded, OutcomeCode(err))
}

// Decoy = operators on every type the holder queries compare, planted ahead
// of pg_catalog, answer false for every pair. Resolved through search_path,
// the table would resolve to nothing and no holder would match, so the wait
// would return at once. With every operator pg_catalog-qualified it reads the
// real catalog and reports the reader that outlasts it.
func TestWaitOutsideLockQueueComparesWithTheCatalogOperators(t *testing.T) {
	f := newHolderWaitFixture(t)
	f.exec(t, "CREATE TABLE %s.t (id int PRIMARY KEY)")
	for _, typ := range []string{"text", "name", "oid", "integer"} {
		f.exec(t, `
			CREATE FUNCTION %[1]s.never(`+typ+`, `+typ+`) RETURNS boolean
				LANGUAGE sql IMMUTABLE AS 'SELECT false';
			CREATE OPERATOR %[1]s.= (LEFTARG = `+typ+`, RIGHTARG = `+typ+`, FUNCTION = %[1]s.never);
			CREATE OPERATOR %[1]s.<> (LEFTARG = `+typ+`, RIGHTARG = `+typ+`, FUNCTION = %[1]s.never)`)
	}
	f.exec(t, `
		CREATE FUNCTION %[1]s.never_concat(text, text) RETURNS text
			LANGUAGE sql IMMUTABLE AS 'SELECT ''decoy''';
		CREATE OPERATOR %[1]s.|| (LEFTARG = text, RIGHTARG = text, FUNCTION = %[1]s.never_concat)`)
	shadowed := testutil.NewCatalogShadowingPool(t, f.url, f.schema)

	var oidEqual, textEqual bool
	require.NoError(t, shadowed.QueryRow(t.Context(),
		`SELECT 1::oid = 1::oid, 'a'::text = 'a'::text`).Scan(&oidEqual, &textEqual))
	require.False(t, oidEqual, "the decoy oid operator must answer a bare = on this pool")
	require.False(t, textEqual, "the decoy text operator must answer a bare = on this pool")

	readerPID := holdLock(t, f.admin, fmt.Sprintf("SELECT count(*) FROM %s.t", f.schema))
	st := parseStatement(t, fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN note text", f.schema))
	err := waitOutsideLockQueue(t.Context(), shadowed, st, f.schema, "t", time.Second, systemHolderWaitClock())
	requireHolderWaitError(t, err, readerPID)
}

// A reader of one partition holds only that partition, not its parent. An
// ALTER TABLE of the parent locks every partition, so it would queue behind
// the reader and waits for it; the same ALTER naming the parent with ONLY
// locks the parent alone and does not wait.
func TestWaitOutsideLockQueueWaitsOnAReaderOfAPartition(t *testing.T) {
	f := newHolderWaitFixture(t)
	f.exec(t, `
		CREATE TABLE %[1]s.p (id int NOT NULL) PARTITION BY RANGE (id);
		CREATE TABLE %[1]s.p_low PARTITION OF %[1]s.p FOR VALUES FROM (0) TO (100)`)
	readerPID := holdLock(t, f.admin, fmt.Sprintf("SELECT count(*) FROM %s.p_low", f.schema))

	recursing := parseStatement(t, fmt.Sprintf("ALTER TABLE %s.p ADD COLUMN note text", f.schema))
	err := waitOutsideLockQueue(t.Context(), f.admin, recursing, f.schema, "p", time.Second, systemHolderWaitClock())
	requireHolderWaitError(t, err, readerPID)

	only := parseStatement(t, fmt.Sprintf("ALTER TABLE ONLY %s.p ALTER COLUMN id SET DEFAULT 0", f.schema))
	require.NoError(t, waitOutsideLockQueue(t.Context(), f.admin, only, f.schema, "p", time.Second, systemHolderWaitClock()))
}

// VALIDATE CONSTRAINT takes SHARE UPDATE EXCLUSIVE, which readers and writers
// never conflict with, so a reader holding the table does not hold it up. A
// transaction holding SHARE UPDATE EXCLUSIVE itself would, and is waited on.
func TestWaitOutsideLockQueueWaitsOnlyOnConflictingModes(t *testing.T) {
	f := newHolderWaitFixture(t)
	f.exec(t, `
		CREATE TABLE %[1]s.t (id int PRIMARY KEY, n int);
		ALTER TABLE %[1]s.t ADD CONSTRAINT n_positive CHECK (n > 0) NOT VALID`)
	validate := parseStatement(t, fmt.Sprintf("ALTER TABLE %s.t VALIDATE CONSTRAINT n_positive", f.schema))

	holdLock(t, f.admin, fmt.Sprintf("SELECT count(*) FROM %s.t", f.schema))
	require.NoError(t, waitOutsideLockQueue(t.Context(), f.admin, validate, f.schema, "t", time.Second, systemHolderWaitClock()),
		"a reader does not conflict with SHARE UPDATE EXCLUSIVE")

	holderPID := holdLock(t, f.admin, fmt.Sprintf("LOCK TABLE %s.t IN SHARE UPDATE EXCLUSIVE MODE", f.schema))
	err := waitOutsideLockQueue(t.Context(), f.admin, validate, f.schema, "t", time.Second, systemHolderWaitClock())
	requireHolderWaitError(t, err, holderPID)
}

// fakeHolderWaitClock advances only when the wait sleeps, and records each
// sleep.
type fakeHolderWaitClock struct {
	at     time.Time
	sleeps []time.Duration
}

func (c *fakeHolderWaitClock) clock() holderWaitClock {
	return holderWaitClock{
		now: func() time.Time { return c.at },
		sleep: func(_ context.Context, d time.Duration) error {
			c.sleeps = append(c.sleeps, d)
			c.at = c.at.Add(d)
			return nil
		},
	}
}

// A holder that never leaves ends the wait exactly at its limit. No sleep
// runs past the deadline, so the last one is cut short, and the error
// reports the limit as the time waited.
func TestWaitOutsideLockQueueStopsAtItsDeadline(t *testing.T) {
	f := newHolderWaitFixture(t)
	f.exec(t, "CREATE TABLE %s.t (id int PRIMARY KEY)")
	readerPID := holdLock(t, f.admin, fmt.Sprintf("SELECT count(*) FROM %s.t", f.schema))
	st := parseStatement(t, fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN note text", f.schema))

	const limit = 500 * time.Millisecond
	fake := &fakeHolderWaitClock{at: time.Unix(0, 0)}
	err := waitOutsideLockQueue(t.Context(), f.admin, st, f.schema, "t", limit, fake.clock())
	requireHolderWaitError(t, err, readerPID)
	var holderErr *LockHolderWaitError
	require.ErrorAs(t, err, &holderErr)
	assert.Equal(t, limit, holderErr.Waited)
	assert.Equal(t, []time.Duration{lockHolderSampleGap, lockHolderSampleGap, limit - 2*lockHolderSampleGap}, fake.sleeps)
}

// The wait's deadline also bounds its catalog reads: a read that starts with
// none of the wait left is cut off by the deadline, and the wait ends with the
// holder error rather than running a read past its limit.
func TestWaitOutsideLockQueueBoundsItsCatalogReads(t *testing.T) {
	f := newHolderWaitFixture(t)
	f.exec(t, "CREATE TABLE %s.t (id int PRIMARY KEY)")
	st := parseStatement(t, fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN note text", f.schema))

	const limit = 300 * time.Millisecond
	start := time.Unix(0, 0)
	reads := 0
	clock := holderWaitClock{
		// The first reading starts the wait; every later one finds the
		// whole limit already spent.
		now: func() time.Time {
			reads++
			if reads == 1 {
				return start
			}
			return start.Add(limit)
		},
		sleep: func(context.Context, time.Duration) error {
			require.FailNow(t, "the wait must not sleep once its deadline has passed")
			return nil
		},
	}
	err := waitOutsideLockQueue(t.Context(), f.admin, st, f.schema, "t", limit, clock)
	var holderErr *LockHolderWaitError
	require.ErrorAs(t, err, &holderErr)
	assert.Equal(t, limit, holderErr.Waited)
}

// The statement decides which holders conflict and whether partitions and
// children count.
func TestHolderScopeOf(t *testing.T) {
	tests := []struct {
		sql         string
		modes       []string
		descendants bool
	}{
		{sql: "ALTER TABLE t ADD COLUMN c int", modes: everyLockMode(), descendants: true},
		{sql: "ALTER TABLE ONLY t ALTER COLUMN c SET DEFAULT 0", modes: everyLockMode(), descendants: false},
		{sql: "ALTER TABLE t VALIDATE CONSTRAINT c", modes: shareUpdateExclusiveConflicts(), descendants: true},
		{sql: "ALTER TABLE t VALIDATE CONSTRAINT c, ADD COLUMN d int", modes: everyLockMode(), descendants: true},
		{sql: "DROP TABLE t", modes: everyLockMode(), descendants: true},
	}
	for _, tt := range tests {
		t.Run(tt.sql, func(t *testing.T) {
			scope, err := holderScopeOf(parseStatement(t, tt.sql))
			require.NoError(t, err)
			assert.Equal(t, tt.modes, scope.modes)
			assert.Equal(t, tt.descendants, scope.descendants)
		})
	}
}
