package executor

import (
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/progress"
)

// These tests run the constraint step against the server: it gives a table
// the parent's CHECK constraints and NOT NULL columns, SET NOT NULL skips its
// scan on the helper CHECK's proof, and the ATTACH that follows, which
// refuses a table without them, goes through.

// noticePool is a pool whose sessions run at client_min_messages = debug1
// under bounded budgets, with every notice the server sends collected, so a
// test can read that SET NOT NULL proved the column from a constraint
// instead of scanning it.
func noticePool(t *testing.T, url string) (*pgxpool.Pool, func() []string) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(url)
	require.NoError(t, err)
	cfg.ConnConfig.RuntimeParams["client_min_messages"] = "debug1"
	cfg.ConnConfig.RuntimeParams["lock_timeout"] = "5s"
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "60s"
	var mu sync.Mutex
	var notices []string
	cfg.ConnConfig.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		mu.Lock()
		defer mu.Unlock()
		notices = append(notices, n.Message)
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), notices...)
	}
}

func (f attachProofFixture) attachTable(t *testing.T, table string) attachTable {
	t.Helper()
	var oid uint32
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT $1::regclass::oid",
		pgx.Identifier{f.schema, table}.Sanitize()).Scan(&oid))
	return attachTable{oid: oid, schema: f.schema, name: table}
}

func (f attachProofFixture) tableOID(t *testing.T, table string) uint32 {
	t.Helper()
	return f.attachTable(t, table).oid
}

// checks maps each CHECK constraint on the table to whether it is validated.
func (f attachProofFixture) checks(t *testing.T, table string) map[string]bool {
	t.Helper()
	rows, err := f.pool.Query(t.Context(), `SELECT conname, convalidated FROM pg_catalog.pg_constraint
		WHERE conrelid = $1::regclass AND contype = 'c'`, pgx.Identifier{f.schema, table}.Sanitize())
	require.NoError(t, err)
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var name string
		var validated bool
		require.NoError(t, rows.Scan(&name, &validated))
		got[name] = validated
	}
	require.NoError(t, rows.Err())
	return got
}

func (f attachProofFixture) notNull(t *testing.T, table, column string) bool {
	t.Helper()
	var notNull bool
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT attnotnull FROM pg_catalog.pg_attribute
		WHERE attrelid = $1::regclass AND attname = $2`, pgx.Identifier{f.schema, table}.Sanitize(), column).Scan(&notNull))
	return notNull
}

// paymentsParent declares two CHECK constraints and two NOT NULL columns.
// payments_2027 has none of them, and no NULL in either column, so the
// attach refuses it until the step has run.
const paymentsParent = `
	CREATE TABLE %[1]s.payments (
		id bigint NOT NULL,
		paid date NOT NULL,
		amount integer,
		currency text,
		CONSTRAINT payments_amount_positive CHECK (amount > 0),
		CONSTRAINT payments_currency_code CHECK (length(currency) = 3)
	) PARTITION BY RANGE (paid);
	CREATE TABLE %[1]s.payments_2027 (
		id bigint,
		paid date,
		amount integer,
		currency text
	);
	INSERT INTO %[1]s.payments_2027 VALUES (1, '2027-01-02', 5, 'AUD'), (2, '2027-06-30', 7, 'USD')`

const attachPayments2027 = `ALTER TABLE %[1]s.payments
	ATTACH PARTITION %[1]s.payments_2027
	FOR VALUES FROM ('2027-01-01') TO ('2028-01-01')`

// From a table with none of the parent's constraints, the step adds both
// CHECKs under the parent's names, validated, and sets both columns NOT NULL
// without a scan, leaving no helper behind. A rerun finds everything, and the
// attach, which refused the table before, goes through.
func TestEnsureAttachConstraintsGivesTheTableTheParentsChecksAndNotNulls(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, paymentsParent)
		_, err := f.pool.Exec(t.Context(), strings.ReplaceAll(attachPayments2027, "%[1]s", pgx.Identifier{f.schema}.Sanitize()))
		require.Error(t, err, "the attach refuses the table before the step")
		pool, notices := noticePool(t, f.url)
		parent, table := f.tableOID(t, "payments"), f.attachTable(t, "payments_2027")

		var ran []string
		results, err := ensureAttachConstraints(t.Context(), pool, parent, table, attachProofTestBudget,
			func(_ progress.Operation, sql string) { ran = append(ran, sql) })
		require.NoError(t, err)
		qualified := pgx.Identifier{f.schema, "payments_2027"}.Sanitize()
		idHelper := table.proof(attachProofNotNull, `"id" IS NOT NULL`, "")
		paidHelper := table.proof(attachProofNotNull, `"paid" IS NOT NULL`, "")
		assert.Equal(t, []string{
			table.proof(attachProofParent, "(amount > 0)", "payments_amount_positive").addSQL(),
			table.proof(attachProofParent, "(amount > 0)", "payments_amount_positive").validateSQL(),
			table.proof(attachProofParent, "(length(currency) = 3)", "payments_currency_code").addSQL(),
			table.proof(attachProofParent, "(length(currency) = 3)", "payments_currency_code").validateSQL(),
			idHelper.addSQL(),
			idHelper.validateSQL(),
			"ALTER TABLE " + qualified + ` ALTER COLUMN "id" SET NOT NULL`,
			"ALTER TABLE " + qualified + " DROP CONSTRAINT " + pgx.Identifier{idHelper.name()}.Sanitize(),
			paidHelper.addSQL(),
			paidHelper.validateSQL(),
			"ALTER TABLE " + qualified + ` ALTER COLUMN "paid" SET NOT NULL`,
			"ALTER TABLE " + qualified + " DROP CONSTRAINT " + pgx.Identifier{paidHelper.name()}.Sanitize(),
		}, ran, "each column's helper is validated before SET NOT NULL and dropped after it")
		assert.Equal(t, []attachConstraintResult{
			{check: "payments_amount_positive", action: attachConstraintAdded},
			{check: "payments_currency_code", action: attachConstraintAdded},
			{column: "id", action: attachConstraintSetNotNull},
			{column: "paid", action: attachConstraintSetNotNull},
		}, results)
		assert.Equal(t, map[string]bool{"payments_amount_positive": true, "payments_currency_code": true},
			f.checks(t, "payments_2027"), "both CHECKs, validated, and no helper left")
		assert.True(t, f.notNull(t, "payments_2027", "id"))
		assert.True(t, f.notNull(t, "payments_2027", "paid"))
		proved := 0
		for _, n := range notices() {
			if strings.Contains(n, "are sufficient to prove that it does not contain nulls") {
				proved++
			}
		}
		assert.Equal(t, 2, proved, "each SET NOT NULL proved its column from the helper CHECK instead of scanning")

		again, err := ensureAttachConstraints(t.Context(), pool, parent, table, attachProofTestBudget, nil)
		require.NoError(t, err)
		for _, r := range again {
			assert.Equal(t, attachConstraintFound, r.action, "a rerun finds %s%s", r.check, r.column)
		}

		f.attach(t, attachPayments2027)
	})
}

// A NULL in a column the parent declares NOT NULL breaks the helper CHECK's
// validation: the step fails typed, the column stays nullable, and the
// helper stays NOT VALID. Once the row is fixed, the next call validates the
// same helper, sets the column NOT NULL, and drops it.
func TestEnsureAttachConstraintsFailsOnANullAndResumesOnceFixed(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, paymentsParent)
		f.exec(t, `INSERT INTO %[1]s.payments_2027 VALUES (NULL, '2027-03-01', 9, 'EUR')`)
		parent, table := f.tableOID(t, "payments"), f.attachTable(t, "payments_2027")
		helper := table.proof(attachProofNotNull, `"id" IS NOT NULL`, "")

		_, err := ensureAttachConstraints(t.Context(), f.pool, parent, table, attachProofTestBudget, nil)
		require.ErrorIs(t, err, errAttachProofViolation)
		assert.False(t, f.notNull(t, "payments_2027", "id"))
		validated, exists := f.proofValidated(t, helper)
		assert.True(t, exists && !validated, "the helper stays, NOT VALID")

		f.exec(t, `UPDATE %[1]s.payments_2027 SET id = 3 WHERE id IS NULL`)
		results, err := ensureAttachConstraints(t.Context(), f.pool, parent, table, attachProofTestBudget, nil)
		require.NoError(t, err)
		assert.Contains(t, results, attachConstraintResult{column: "id", action: attachConstraintSetNotNull})
		assert.True(t, f.notNull(t, "payments_2027", "id"))
		_, exists = f.proofValidated(t, helper)
		assert.False(t, exists, "the helper is dropped")
	})
}

// A stop after SET NOT NULL and before the helper's drop leaves the column
// NOT NULL and the helper in place. The next call finds the column and drops
// the helper.
func TestEnsureAttachConstraintsDropsAHelperAStopLeft(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, `
			CREATE TABLE %[1]s.events (
				id bigint NOT NULL,
				day date
			) PARTITION BY RANGE (day);
			CREATE TABLE %[1]s.events_2027 (
				id bigint,
				day date
			)`)
		table := f.attachTable(t, "events_2027")
		helper := table.proof(attachProofNotNull, `"id" IS NOT NULL`, "")
		f.prove(t, "events_2027", helper.name(), helper.expr)
		f.exec(t, `ALTER TABLE %[1]s.events_2027 ALTER COLUMN id SET NOT NULL`)

		results, err := ensureAttachConstraints(t.Context(), f.pool, f.tableOID(t, "events"), table, attachProofTestBudget, nil)
		require.NoError(t, err)
		assert.Equal(t, []attachConstraintResult{{column: "id", action: attachConstraintFound}}, results)
		_, exists := f.proofValidated(t, helper)
		assert.False(t, exists, "the leftover helper is dropped")
	})
}

// A CHECK under the parent's name with another expression is not the
// parent's CHECK, and is not replaced; a table that lacks a NOT NULL column
// of the parent is refused before anything is set.
func TestEnsureAttachConstraintsRefusesAMismatchedTable(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		t.Run("a CHECK under the parent's name with another expression", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, paymentsParent)
			f.exec(t, `ALTER TABLE %[1]s.payments_2027 ADD CONSTRAINT payments_amount_positive CHECK (amount >= 0)`)
			_, err := ensureAttachConstraints(t.Context(), f.pool, f.tableOID(t, "payments"), f.attachTable(t, "payments_2027"), attachProofTestBudget, nil)
			require.ErrorIs(t, err, errAttachProofNameTaken)
			assert.Equal(t, map[string]bool{"payments_amount_positive": true}, f.checks(t, "payments_2027"), "left as it was")
		})

		t.Run("a NOT NULL column the table lacks", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, `
				CREATE TABLE %[1]s.events (
					id bigint NOT NULL,
					day date
				) PARTITION BY RANGE (day);
				CREATE TABLE %[1]s.events_2027 (
					day date
				)`)
			_, err := ensureAttachConstraints(t.Context(), f.pool, f.tableOID(t, "events"), f.attachTable(t, "events_2027"), attachProofTestBudget, nil)
			require.ErrorIs(t, err, errAttachColumnMissing)
			assert.Empty(t, f.checks(t, "events_2027"))
		})
	})
}
