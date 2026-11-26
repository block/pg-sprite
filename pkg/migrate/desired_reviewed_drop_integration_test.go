package migrate_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// reviewedDropFixture is a table with one of everything a desired file can
// ask to drop: a column, a CHECK constraint, a NOT NULL, and an index.
const reviewedDropFixture = `CREATE TABLE %[1]s.t (
	id int PRIMARY KEY,
	keep text,
	legacy text,
	qty int NOT NULL,
	CONSTRAINT t_qty_positive CHECK (qty > 0)
);
CREATE INDEX t_keep_idx ON %[1]s.t (keep);
INSERT INTO %[1]s.t VALUES (1, 'k', 'l', 3)`

func reviewedDropTable(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(reviewedDropFixture, schema))
	require.NoError(t, err)
	return pool, schema
}

func plannedDrop(t *testing.T, pool *pgxpool.Pool, schema, desiredSQL string) plan.Report {
	t.Helper()
	report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: parseDesired(t, desiredSQL)})
	require.NoError(t, err)
	return report
}

func liveColumns(t *testing.T, pool *pgxpool.Pool, schema string) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(),
		`SELECT column_name FROM information_schema.columns
		  WHERE table_schema = $1 AND table_name = 't' ORDER BY ordinal_position`, schema)
	require.NoError(t, err)
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		require.NoError(t, rows.Scan(&c))
		cols = append(cols, c)
	}
	require.NoError(t, rows.Err())
	return cols
}

func liveIndexes(t *testing.T, pool *pgxpool.Pool, schema string) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(),
		`SELECT indexname FROM pg_indexes WHERE schemaname = $1 AND tablename = 't' ORDER BY indexname`, schema)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		out = append(out, n)
	}
	require.NoError(t, rows.Err())
	return out
}

// holdTableRead opens a transaction that has read the table, which holds a
// DROP INDEX CONCURRENTLY of its index at the wait for older snapshots until
// release is called, without blocking any other statement on the table.
func holdTableRead(t *testing.T, pool *pgxpool.Pool, schema string) (release func()) {
	t.Helper()
	_, release = holdTableReadPID(t, pool, schema)
	return release
}

// holdTableReadPID is holdTableRead that also returns the holding session's
// backend PID, so a test can wait until the drop is waiting on it.
func holdTableReadPID(t *testing.T, pool *pgxpool.Pool, schema string) (pid uint32, release func()) {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	var n int
	require.NoError(t, tx.QueryRow(t.Context(), fmt.Sprintf("SELECT count(*) FROM %s.t", schema)).Scan(&n))
	released := false
	release = func() {
		if !released {
			released = true
			assert.NoError(t, tx.Rollback(context.WithoutCancel(t.Context())))
		}
	}
	t.Cleanup(release)
	return tx.Conn().PgConn().PID(), release
}

// An operator reviews a desired file that removes a column, a constraint, a
// NOT NULL, and an index, pins the plan they reviewed, and names the table in
// the destructive acknowledgement. Each destructive statement runs, its
// verdict records the acknowledgement, the table converges on the file, and re-running the same request plans nothing.
func TestRunDesiredAppliesAReviewedDrop(t *testing.T) {
	cases := []struct {
		name    string
		kind    statement.OpKind
		desired string
		check   func(t *testing.T, pool *pgxpool.Pool, schema string)
		sqlHas  string
	}{
		{
			name: "drop column",
			kind: statement.OpDropColumn,
			desired: `CREATE TABLE t (
				id int PRIMARY KEY,
				keep text,
				qty int NOT NULL,
				CONSTRAINT t_qty_positive CHECK (qty > 0)
			);
			CREATE INDEX t_keep_idx ON t (keep);`,
			check: func(t *testing.T, pool *pgxpool.Pool, schema string) {
				assert.Equal(t, []string{"id", "keep", "qty"}, liveColumns(t, pool, schema))
			},
			sqlHas: "DROP legacy",
		},
		{
			name: "drop constraint",
			kind: statement.OpDropConstraint,
			desired: `CREATE TABLE t (
				id int PRIMARY KEY,
				keep text,
				legacy text,
				qty int NOT NULL
			);
			CREATE INDEX t_keep_idx ON t (keep);`,
			check: func(t *testing.T, pool *pgxpool.Pool, schema string) {
				var n int
				require.NoError(t, pool.QueryRow(t.Context(),
					`SELECT count(*) FROM pg_constraint WHERE conrelid = $1::regclass AND conname = 't_qty_positive'`,
					schema+".t").Scan(&n))
				assert.Zero(t, n)
			},
			sqlHas: "DROP CONSTRAINT",
		},
		{
			name: "drop not null",
			kind: statement.OpDropNotNull,
			desired: `CREATE TABLE t (
				id int PRIMARY KEY,
				keep text,
				legacy text,
				qty int,
				CONSTRAINT t_qty_positive CHECK (qty > 0)
			);
			CREATE INDEX t_keep_idx ON t (keep);`,
			check: func(t *testing.T, pool *pgxpool.Pool, schema string) {
				var nullable bool
				require.NoError(t, pool.QueryRow(t.Context(),
					`SELECT is_nullable = 'YES' FROM information_schema.columns
					  WHERE table_schema = $1 AND table_name = 't' AND column_name = 'qty'`, schema).Scan(&nullable))
				assert.True(t, nullable)
			},
			sqlHas: "DROP NOT NULL",
		},
		{
			name: "drop index",
			kind: statement.OpDropIndex,
			desired: `CREATE TABLE t (
				id int PRIMARY KEY,
				keep text,
				legacy text,
				qty int NOT NULL,
				CONSTRAINT t_qty_positive CHECK (qty > 0)
			);`,
			check: func(t *testing.T, pool *pgxpool.Pool, schema string) {
				assert.Equal(t, []string{"t_pkey"}, liveIndexes(t, pool, schema))
			},
			sqlHas: "DROP INDEX",
		},
	}
	// Every shape the planner marks destructive has a case, so a new one
	// fails here until a reviewed desired file has run it. A desired file
	// declares one table and none of its partitions, so it cannot express a
	// partition detach.
	var covered []statement.OpKind
	for _, tc := range cases {
		covered = append(covered, tc.kind)
	}
	expressible := slices.DeleteFunc(planner.DestructiveOpKinds(), func(k statement.OpKind) bool {
		return k == statement.OpDetachPartition
	})
	require.ElementsMatch(t, expressible, covered)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool, schema := reviewedDropTable(t)
			reviewed := plannedDrop(t, pool, schema, tc.desired)
			require.Len(t, reviewed.Statements, 1)
			require.True(t, reviewed.Statements[0].Destructive)
			require.Contains(t, reviewed.Statements[0].SQL, tc.sqlHas)

			opts := runOptions()
			req := migrate.DesiredRequest{
				Schema:              schema,
				Desired:             parseDesired(t, tc.desired),
				ExpectedFingerprint: reviewed.Fingerprint,
				AllowDestructive:    schema + ".t",
			}
			res, err := migrate.RunDesired(t.Context(), pool, req, opts)
			require.NoError(t, err)
			require.Equal(t, verdict.OutcomeExecuted, res.Outcome, "%s", res.Detail)
			require.Len(t, res.Verdicts, 1)
			assert.Equal(t, verdict.OutcomeExecuted, res.Verdicts[0].Outcome)
			assert.True(t, res.Verdicts[0].AllowedDestructive, "the verdict records the destructive acknowledgement")
			assert.Equal(t, reviewed.Fingerprint, res.Plan.Fingerprint, "the executed plan is the reviewed plan")
			if tc.kind == statement.OpDropIndex {
				assert.Equal(t, reviewed.Statements[0].ExecSQL, res.Verdicts[0].ExecutedSQL, "the index drop runs as the concurrent statement the plan named")
			}
			tc.check(t, pool, schema)

			// The table now matches the file: the same request plans nothing.
			res, err = migrate.RunDesired(t.Context(), pool, req, opts)
			require.NoError(t, err)
			assert.Equal(t, verdict.OutcomeExecuted, res.Outcome)
			assert.Empty(t, res.Plan.Statements)
			assert.Empty(t, res.Verdicts)
		})
	}
}

// A plan that mixes a non-destructive statement with a drop runs both under
// one acknowledgement, and only the drop's verdict is marked as acknowledged:
// the added column's verdict does not claim a loss it did not cause.
func TestRunDesiredMarksOnlyTheDestructiveStatementAcknowledged(t *testing.T) {
	pool, schema := reviewedDropTable(t)
	desired := `CREATE TABLE t (
		id int PRIMARY KEY,
		keep text,
		extra int,
		qty int NOT NULL,
		CONSTRAINT t_qty_positive CHECK (qty > 0)
	);
	CREATE INDEX t_keep_idx ON t (keep);`
	reviewed := plannedDrop(t, pool, schema, desired)
	require.Len(t, reviewed.Statements, 2)
	var destructive []string
	for _, ps := range reviewed.Statements {
		if ps.Destructive {
			destructive = append(destructive, ps.SQL)
		}
	}
	require.Len(t, destructive, 1, "exactly one planned statement is destructive")
	require.Contains(t, destructive[0], "DROP legacy")

	res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{
		Schema: schema, Desired: parseDesired(t, desired),
		ExpectedFingerprint: reviewed.Fingerprint, AllowDestructive: schema + ".t",
	}, runOptions())
	require.NoError(t, err)
	require.Equal(t, verdict.OutcomeExecuted, res.Outcome, "%s", res.Detail)
	require.Len(t, res.Verdicts, 2)
	for i, ps := range reviewed.Statements {
		assert.Equal(t, ps.Destructive, res.Verdicts[i].AllowedDestructive, "statement %q", ps.SQL)
	}
	assert.Equal(t, []string{"id", "keep", "qty", "extra"}, liveColumns(t, pool, schema))
}

// Without the acknowledgement a destructive plan is refused whole, nothing
// runs (the plan's non-destructive statement included), and the refusal names
// the fingerprint to pin and the table to acknowledge. An acknowledgement
// naming another table is a usage error that runs nothing.
func TestRunDesiredRefusesAnUnreviewedOrMismatchedDrop(t *testing.T) {
	pool, schema := reviewedDropTable(t)
	desired := `CREATE TABLE t (
		id int PRIMARY KEY,
		keep text,
		extra int,
		qty int NOT NULL,
		CONSTRAINT t_qty_positive CHECK (qty > 0)
	);
	CREATE INDEX t_keep_idx ON t (keep);`
	reviewed := plannedDrop(t, pool, schema, desired)
	ds := parseDesired(t, desired)
	untouched := func(t *testing.T) {
		t.Helper()
		assert.Equal(t, []string{"id", "keep", "legacy", "qty"}, liveColumns(t, pool, schema), "nothing may have run")
	}

	t.Run("no acknowledgement", func(t *testing.T) {
		res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{
			Schema: schema, Desired: ds, ExpectedFingerprint: reviewed.Fingerprint,
		}, runOptions())
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
		assert.Equal(t, verdict.ReasonDestructiveChange, res.Reason)
		assert.Empty(t, res.Verdicts)
		assert.Contains(t, res.Detail, "ExpectedFingerprint "+reviewed.Fingerprint)
		assert.Contains(t, res.Detail, "AllowDestructive "+schema+".t")
		untouched(t)
	})

	t.Run("acknowledgement naming another table", func(t *testing.T) {
		_, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{
			Schema: schema, Desired: ds, ExpectedFingerprint: reviewed.Fingerprint, AllowDestructive: schema + ".other",
		}, runOptions())
		require.ErrorIs(t, err, migrate.ErrAllowDestructiveMismatch)
		untouched(t)
	})
}

// The row-security front door forwards the acknowledgement to the table
// definition's plan, and refuses an acknowledgement with no pinned
// fingerprint before any database work.
func TestRunDesiredWithRowSecurityAppliesAReviewedDrop(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN legacy text", schema))
	require.NoError(t, err)
	desired := parseRowSecurity(t, fmt.Sprintf(documentsWithRowSecurity, "", ""))
	planned, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: desired.TableDefinition()})
	require.NoError(t, err)
	require.Len(t, planned.Statements, 1)
	require.True(t, planned.Statements[0].Destructive)

	_, err = migrate.RunDesiredWithRowSecurity(t.Context(), pool, migrate.RowSecurityRequest{
		Schema: schema, Desired: desired, AllowDestructive: schema + ".t",
	}, runOptions())
	require.ErrorIs(t, err, migrate.ErrAllowDestructiveNeedsFingerprint)

	res, err := migrate.RunDesiredWithRowSecurity(t.Context(), pool, migrate.RowSecurityRequest{
		Schema: schema, Desired: desired, ExpectedFingerprint: planned.Fingerprint, AllowDestructive: schema + ".t",
	}, runOptions())
	require.NoError(t, err)
	require.Equal(t, verdict.OutcomeExecuted, res.Outcome, "%s", res.Detail)
	assert.Equal(t, []string{"id", "owner_id"}, liveColumns(t, pool, schema))
}

// The replica identity index of a table cannot be dropped by a reviewed
// plan: dropping it makes UPDATE and DELETE fail while a publication
// publishes them. The planner routes that drop to a refusal, so even a pinned
// and acknowledged request refuses the plan whole before anything runs, and
// the index stays.
func TestRunDesiredReviewedIndexDropRefusesTheReplicaIdentityIndex(t *testing.T) {
	pool, schema := reviewedDropTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`ALTER TABLE %[1]s.t ALTER COLUMN keep SET NOT NULL;
CREATE UNIQUE INDEX t_ident_idx ON %[1]s.t (keep);
ALTER TABLE %[1]s.t REPLICA IDENTITY USING INDEX t_ident_idx`, schema))
	require.NoError(t, err)
	desired := `CREATE TABLE t (
		id int PRIMARY KEY,
		keep text NOT NULL,
		legacy text,
		qty int NOT NULL,
		CONSTRAINT t_qty_positive CHECK (qty > 0)
	);
	CREATE INDEX t_keep_idx ON t (keep);`
	reviewed := plannedDrop(t, pool, schema, desired)
	require.Len(t, reviewed.Statements, 1)
	require.Contains(t, reviewed.Statements[0].SQL, "t_ident_idx")

	res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{
		Schema: schema, Desired: parseDesired(t, desired),
		ExpectedFingerprint: reviewed.Fingerprint, AllowDestructive: schema + ".t",
	}, runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
	assert.Empty(t, res.Verdicts, "the plan is refused before any statement runs")
	assert.Contains(t, liveIndexes(t, pool, schema), "t_ident_idx")
}

// The pin binds what a drop removes, not only its name. After a drop is
// reviewed, the object it names is replaced by another with the same name:
// an index with other columns, a column dropped and added again, a
// constraint with another check, a NOT NULL on a column added again, an
// index added on the column a drop removes. The plan a fresh run derives
// drops the same objects, yet has another fingerprint, so the old pin is
// refused as stale and nothing is dropped.
func TestRunDesiredPinBindsTheDefinitionOfWhatADropRemoves(t *testing.T) {
	cases := []struct {
		name    string
		desired string
		replace string
		// extra is set when the replacement also plans a statement of its own.
		extra  bool
		intact func(t *testing.T, pool *pgxpool.Pool, schema string)
	}{
		{
			name: "index replaced under its name",
			desired: `CREATE TABLE t (
				id int PRIMARY KEY,
				keep text,
				legacy text,
				qty int NOT NULL,
				CONSTRAINT t_qty_positive CHECK (qty > 0)
			);`,
			replace: `DROP INDEX %[1]s.t_keep_idx; CREATE INDEX t_keep_idx ON %[1]s.t (keep, legacy)`,
			intact: func(t *testing.T, pool *pgxpool.Pool, schema string) {
				assert.Equal(t, []string{"t_keep_idx", "t_pkey"}, liveIndexes(t, pool, schema))
			},
		},
		{
			name: "column dropped and added again",
			desired: `CREATE TABLE t (
				id int PRIMARY KEY,
				keep text,
				qty int NOT NULL,
				CONSTRAINT t_qty_positive CHECK (qty > 0)
			);
			CREATE INDEX t_keep_idx ON t (keep);`,
			replace: `ALTER TABLE %[1]s.t DROP COLUMN legacy; ALTER TABLE %[1]s.t ADD COLUMN legacy text`,
			intact: func(t *testing.T, pool *pgxpool.Pool, schema string) {
				assert.Contains(t, liveColumns(t, pool, schema), "legacy")
			},
		},
		{
			name: "constraint name reused",
			desired: `CREATE TABLE t (
				id int PRIMARY KEY,
				keep text,
				legacy text,
				qty int NOT NULL
			);
			CREATE INDEX t_keep_idx ON t (keep);`,
			replace: `ALTER TABLE %[1]s.t DROP CONSTRAINT t_qty_positive; ALTER TABLE %[1]s.t ADD CONSTRAINT t_qty_positive CHECK (qty > 2)`,
			intact: func(t *testing.T, pool *pgxpool.Pool, schema string) {
				var def string
				require.NoError(t, pool.QueryRow(t.Context(),
					`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid = $1::regclass AND conname = 't_qty_positive'`,
					schema+".t").Scan(&def))
				assert.Contains(t, def, "2")
			},
		},
		{
			name: "NOT NULL on a column added again",
			desired: `CREATE TABLE t (
				id int PRIMARY KEY,
				keep text,
				legacy text,
				qty int,
				CONSTRAINT t_qty_positive CHECK (qty > 0)
			);
			CREATE INDEX t_keep_idx ON t (keep);`,
			replace: `ALTER TABLE %[1]s.t DROP CONSTRAINT t_qty_positive; ALTER TABLE %[1]s.t DROP COLUMN qty;
				ALTER TABLE %[1]s.t ADD COLUMN qty int NOT NULL DEFAULT 3; ALTER TABLE %[1]s.t ALTER COLUMN qty DROP DEFAULT;
				ALTER TABLE %[1]s.t ADD CONSTRAINT t_qty_positive CHECK (qty > 0)`,
			intact: func(t *testing.T, pool *pgxpool.Pool, schema string) {
				var notNull bool
				require.NoError(t, pool.QueryRow(t.Context(),
					`SELECT attnotnull FROM pg_attribute WHERE attrelid = $1::regclass AND attname = 'qty'`,
					schema+".t").Scan(&notNull))
				assert.True(t, notNull)
			},
		},
		{
			name: "index added on the dropped column",
			desired: `CREATE TABLE t (
				id int PRIMARY KEY,
				keep text,
				qty int NOT NULL,
				CONSTRAINT t_qty_positive CHECK (qty > 0)
			);
			CREATE INDEX t_keep_idx ON t (keep);`,
			replace: `CREATE INDEX t_legacy_idx ON %[1]s.t (legacy)`,
			extra:   true,
			intact: func(t *testing.T, pool *pgxpool.Pool, schema string) {
				assert.Contains(t, liveColumns(t, pool, schema), "legacy")
				assert.Contains(t, liveIndexes(t, pool, schema), "t_legacy_idx")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool, schema := reviewedDropTable(t)
			reviewed := plannedDrop(t, pool, schema, tc.desired)
			require.NotEmpty(t, reviewed.Statements)

			_, err := pool.Exec(t.Context(), fmt.Sprintf(tc.replace, schema))
			require.NoError(t, err)
			fresh := plannedDrop(t, pool, schema, tc.desired)
			if !tc.extra {
				require.Equal(t, statementSQL(reviewed), statementSQL(fresh),
					"the replacement must plan the same statements, so only the definition differs")
			}
			assert.NotEqual(t, reviewed.Fingerprint, fresh.Fingerprint,
				"a change to what the drop removes must change the plan's identity")

			res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{
				Schema: schema, Desired: parseDesired(t, tc.desired),
				ExpectedFingerprint: reviewed.Fingerprint, AllowDestructive: schema + ".t",
			}, runOptions())
			require.NoError(t, err)
			assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
			assert.Equal(t, verdict.ReasonPlanFingerprintMismatch, res.Reason)
			assert.Empty(t, res.Verdicts)
			tc.intact(t, pool, schema)
		})
	}
}

func statementSQL(report plan.Report) []string {
	out := make([]string, len(report.Statements))
	for i, ps := range report.Statements {
		out[i] = ps.SQL
	}
	return out
}

// A reviewed index drop that the server cancels at its statement timeout
// fails like any other statement the server failed: the verdict carries the
// server's SQLSTATE and message, so a caller reading the JSON can tell a
// cancelled drop from an unreachable one. A transaction that has read the
// table holds the drop at its wait until the timeout.
func TestRunDesiredReviewedIndexDropFailureCarriesTheServerError(t *testing.T) {
	pool, schema := reviewedDropTable(t)
	desired := `CREATE TABLE t (
		id int PRIMARY KEY,
		keep text,
		legacy text,
		qty int NOT NULL,
		CONSTRAINT t_qty_positive CHECK (qty > 0)
	);`
	reviewed := plannedDrop(t, pool, schema, desired)
	require.Len(t, reviewed.Statements, 1)

	holdTableRead(t, pool, schema)

	opts := runOptions()
	opts.Budget.Concurrent = executor.ConcurrentBudget{Overall: 300 * time.Millisecond}
	res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{
		Schema: schema, Desired: parseDesired(t, desired),
		ExpectedFingerprint: reviewed.Fingerprint, AllowDestructive: schema + ".t",
	}, opts)
	require.Error(t, err)
	assert.Equal(t, verdict.OutcomeFailed, res.Outcome)
	require.Len(t, res.Verdicts, 1)
	v := res.Verdicts[0]
	assert.Equal(t, "57014", v.SQLState, "the verdict names the server's query_canceled SQLSTATE")
	assert.Contains(t, liveIndexes(t, pool, schema), "t_keep_idx", "the cancelled drop leaves the index in the catalog")
	_, err = pool.Exec(t.Context(), fmt.Sprintf("INSERT INTO %s.t VALUES (2, 'k2', 'l2', 4)", schema))
	require.NoError(t, err, "the table stays writable after the cancelled drop")

	raw, err := json.Marshal(v)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"sqlstate":"57014"`)
	assert.Contains(t, string(raw), `"server_error":`)
}

// A reviewed index drop never blocks writes. While the drop waits for an
// older transaction that has read the table, another session still writes
// under a short lock timeout; once the reader ends, the drop finishes and
// the index is gone.
func TestRunDesiredReviewedIndexDropLeavesWritesUnblocked(t *testing.T) {
	pool, schema := reviewedDropTable(t)
	desired := `CREATE TABLE t (
		id int PRIMARY KEY,
		keep text,
		legacy text,
		qty int NOT NULL,
		CONSTRAINT t_qty_positive CHECK (qty > 0)
	);`
	reviewed := plannedDrop(t, pool, schema, desired)
	require.Len(t, reviewed.Statements, 1)
	holder, release := holdTableReadPID(t, pool, schema)
	desiredSchema := parseDesired(t, desired)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	type outcome struct {
		res migrate.DesiredResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := migrate.RunDesired(ctx, pool, migrate.DesiredRequest{
			Schema: schema, Desired: desiredSchema,
			ExpectedFingerprint: reviewed.Fingerprint, AllowDestructive: schema + ".t",
		}, runOptions())
		done <- outcome{res, err}
	}()
	require.Eventually(t, func() bool {
		blocked, err := testutil.BlockedBy(t.Context(), pool, holder)
		return err == nil && blocked
	}, 30*time.Second, 25*time.Millisecond, "the drop must be waiting on the older reader")

	writer, err := pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { _ = writer.Rollback(context.WithoutCancel(t.Context())) }()
	_, err = writer.Exec(t.Context(), "SET LOCAL lock_timeout = '200ms'")
	require.NoError(t, err)
	_, err = writer.Exec(t.Context(), fmt.Sprintf("INSERT INTO %s.t VALUES (2, 'k2', 'l2', 4)", schema))
	require.NoError(t, err, "a write must not wait on the pending drop")
	require.NoError(t, writer.Commit(t.Context()))

	release()
	select {
	case out := <-done:
		require.NoError(t, out.err)
		require.Equal(t, verdict.OutcomeExecuted, out.res.Outcome, "%s", out.res.Detail)
	case <-time.After(30 * time.Second):
		cancel()
		t.Fatal("the drop must finish once the reader ends")
	}
	assert.Equal(t, []string{"t_pkey"}, liveIndexes(t, pool, schema))
}
