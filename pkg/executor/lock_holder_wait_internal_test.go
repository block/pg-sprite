// White-box tests of the wait a step takes outside the lock queue: which
// holders it waits on, that it reads the real catalog on a session whose
// search_path lists a decoy schema ahead of pg_catalog, that one deadline
// bounds the whole wait, and what it reports while it waits.

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
	"github.com/block/pg-sprite/pkg/progress"
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

// waitFor runs the wait for sql, formatted with the fixture's schema, on table.
func (f holderWaitFixture) waitFor(t *testing.T, pool *pgxpool.Pool, sql, table string, limit time.Duration, clock holderWaitClock) holderWaitOutcome {
	t.Helper()
	tables, err := nativeLockedTables(parseStatement(t, fmt.Sprintf(sql, f.schema)), f.schema, table)
	require.NoError(t, err)
	outcome, err := waitOutsideLockQueue(t.Context(), pool, tables, limit, clock, func(*progress.LockHolderWait) {})
	require.NoError(t, err)
	return outcome
}

// requireOutlasted asserts the wait ran out with pid still holding its lock.
func requireOutlasted(t *testing.T, outcome holderWaitOutcome, pid int32) {
	t.Helper()
	require.True(t, outcome.exhausted, "the holder outlasts the wait")
	assert.Equal(t, pid, outcome.holder.pid)
}

// Decoy = operators on every type the holder queries compare, planted ahead
// of pg_catalog, answer false for every pair. Resolved through search_path,
// the table would resolve to nothing and no holder would match, so the wait
// would return at once. With every operator pg_catalog-qualified it reads the
// real catalog and waits out the reader.
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
	requireOutlasted(t, f.waitFor(t, shadowed, "ALTER TABLE %s.t ADD COLUMN note text", "t", time.Second, systemHolderWaitClock()), readerPID)
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

	requireOutlasted(t, f.waitFor(t, f.admin, "ALTER TABLE %s.p ADD COLUMN note text", "p", time.Second, systemHolderWaitClock()), readerPID)
	outcome := f.waitFor(t, f.admin, "ALTER TABLE ONLY %s.p ALTER COLUMN id SET DEFAULT 0", "p", time.Second, systemHolderWaitClock())
	assert.False(t, outcome.exhausted, "ONLY locks the parent alone")
}

// Only application reads and writes are waited out. A transaction holding
// SHARE UPDATE EXCLUSIVE, as VACUUM and autovacuum do, is not: waiting would
// not outlast it, and autovacuum gives way to the lock request by itself.
// Adding a foreign key locks the referenced table against writes only, so a
// writer there is waited out and a reader is not.
func TestWaitOutsideLockQueueWaitsOnlyOnReadsAndWrites(t *testing.T) {
	f := newHolderWaitFixture(t)
	f.exec(t, `
		CREATE TABLE %[1]s.t (id int PRIMARY KEY);
		CREATE TABLE %[1]s.users (id int PRIMARY KEY);
		CREATE TABLE %[1]s.orders (id int PRIMARY KEY, user_id int)`)
	addFK := "ALTER TABLE %s.orders ADD CONSTRAINT orders_user_fk FOREIGN KEY (user_id) REFERENCES users (id) NOT VALID"

	holdLock(t, f.admin, fmt.Sprintf("LOCK TABLE %s.t IN SHARE UPDATE EXCLUSIVE MODE", f.schema))
	assert.False(t, f.waitFor(t, f.admin, "ALTER TABLE %s.t ADD COLUMN note text", "t", time.Second, systemHolderWaitClock()).exhausted,
		"a maintenance lock is not waited on")

	holdLock(t, f.admin, fmt.Sprintf("SELECT count(*) FROM %s.users", f.schema))
	assert.False(t, f.waitFor(t, f.admin, addFK, "orders", time.Second, systemHolderWaitClock()).exhausted,
		"a reader of the referenced table does not conflict with SHARE ROW EXCLUSIVE")

	writerPID := holdLock(t, f.admin, fmt.Sprintf("INSERT INTO %s.users VALUES (1)", f.schema))
	outcome := f.waitFor(t, f.admin, addFK, "orders", time.Second, systemHolderWaitClock())
	requireOutlasted(t, outcome, writerPID)
	assert.Equal(t, qualifiedName(f.schema, "users"), outcome.holder.table)
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
// runs past the deadline, so the last one is cut short, and the sample after
// it, with none of the wait left, ends the wait. While the holder lingers,
// each sample reports it with the time waited, and the report clears when the
// wait ends.
func TestWaitOutsideLockQueueStopsAtItsDeadline(t *testing.T) {
	f := newHolderWaitFixture(t)
	f.exec(t, "CREATE TABLE %s.t (id int PRIMARY KEY)")
	readerPID := holdLock(t, f.admin, fmt.Sprintf("SELECT count(*) FROM %s.t", f.schema))
	tables, err := nativeLockedTables(parseStatement(t, fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN note text", f.schema)), f.schema, "t")
	require.NoError(t, err)

	const limit = 500 * time.Millisecond
	fake := &fakeHolderWaitClock{at: time.Unix(0, 0)}
	var reports []*progress.LockHolderWait
	outcome, err := waitOutsideLockQueue(t.Context(), f.admin, tables, limit, fake.clock(), func(w *progress.LockHolderWait) {
		reports = append(reports, w)
	})
	require.NoError(t, err)
	requireOutlasted(t, outcome, readerPID)
	assert.Equal(t, limit, outcome.waited)
	assert.Equal(t, []time.Duration{lockHolderSampleGap, lockHolderSampleGap, limit - 2*lockHolderSampleGap}, fake.sleeps)
	table := qualifiedName(f.schema, "t")
	assert.Equal(t, []*progress.LockHolderWait{
		{PID: uint32(readerPID), Table: table, Waited: lockHolderSampleGap, Limit: limit},
		{PID: uint32(readerPID), Table: table, Waited: 2 * lockHolderSampleGap, Limit: limit},
		nil,
	}, reports)
}

// The wait's deadline also bounds its catalog reads: a read that starts with
// none of the wait left is cut off by the deadline, and the wait ends as run
// out rather than running a read past its limit.
func TestWaitOutsideLockQueueBoundsItsCatalogReads(t *testing.T) {
	f := newHolderWaitFixture(t)
	f.exec(t, "CREATE TABLE %s.t (id int PRIMARY KEY)")

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
	outcome := f.waitFor(t, f.admin, "ALTER TABLE %s.t ADD COLUMN note text", "t", limit, clock)
	assert.True(t, outcome.exhausted)
	assert.Equal(t, limit, outcome.waited)
}

// The statement decides which tables the step locks, which holders conflict
// on each, and whether partitions and children count.
func TestNativeLockedTables(t *testing.T) {
	readWrite, write := readAndWriteLockModes(), writeLockModes()
	tests := []struct {
		sql  string
		want []lockedTable
	}{
		{sql: "ALTER TABLE t ADD COLUMN c int", want: []lockedTable{{"s", "t", true, readWrite}}},
		{sql: "ALTER TABLE ONLY t ALTER COLUMN c SET DEFAULT 0", want: []lockedTable{{"s", "t", false, readWrite}}},
		{sql: "ALTER TABLE t VALIDATE CONSTRAINT c"},
		{sql: "ALTER TABLE t VALIDATE CONSTRAINT c, ADD COLUMN d int", want: []lockedTable{{"s", "t", true, readWrite}}},
		{
			sql:  "ALTER TABLE t ADD CONSTRAINT fk FOREIGN KEY (u) REFERENCES app.users (id) NOT VALID",
			want: []lockedTable{{"s", "t", true, write}, {"app", "users", true, write}},
		},
		{
			sql:  "ALTER TABLE t ADD COLUMN u int REFERENCES users (id)",
			want: []lockedTable{{"s", "t", true, readWrite}, {"s", "users", true, write}},
		},
		{
			sql:  "ALTER TABLE t ADD CONSTRAINT fk FOREIGN KEY (p) REFERENCES t (id) NOT VALID",
			want: []lockedTable{{"s", "t", true, write}},
		},
		{sql: "DROP TABLE t", want: []lockedTable{{"s", "t", true, readWrite}}},
	}
	for _, tt := range tests {
		t.Run(tt.sql, func(t *testing.T) {
			tables, err := nativeLockedTables(parseStatement(t, tt.sql), "s", "t")
			require.NoError(t, err)
			assert.Equal(t, tt.want, tables)
		})
	}
}

// A lingering holder is named with what the role could see of it: its state
// and transaction age when pg_stat_activity shows the session, and only its
// backend when it does not.
func TestLingeringHolderErrorNamesWhatTheRoleCouldSee(t *testing.T) {
	seen := &LingeringHolderError{PID: 4242, Table: "billing.invoices", Watched: 30 * time.Second, State: "active", TransactionAge: 2*time.Hour + 400*time.Millisecond}
	assert.Equal(t, "backend 4242 (active, transaction open for 2h0m0s) held a lock on billing.invoices for the whole 30s the step waited, "+
		"so the step did not request its lock and no reader or writer queued behind it", seen.Error())

	hidden := &LingeringHolderError{PID: 4242, Table: "billing.invoices", Watched: 30 * time.Second}
	assert.Equal(t, "backend 4242 held a lock on billing.invoices for the whole 30s the step waited, "+
		"so the step did not request its lock and no reader or writer queued behind it", hidden.Error())
}
