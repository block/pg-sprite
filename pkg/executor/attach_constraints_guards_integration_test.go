package executor

import (
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/progress"
)

// These tests run the constraint step where the table or the parent is not
// what the step would build on: a CHECK the step did not create under a name
// it uses, a column it cannot prove, a parent that is gone or changes while
// it runs, and a session holding the table before SET NOT NULL.

// eventsParent declares one NOT NULL column. events_2027 allows NULL in it,
// so the step gives it the helper CHECK and sets it NOT NULL.
const eventsParent = `
	CREATE TABLE %[1]s.events (
		id bigint NOT NULL,
		day date
	) PARTITION BY RANGE (day);
	CREATE TABLE %[1]s.events_2027 (
		id bigint,
		day date
	);
	INSERT INTO %[1]s.events_2027 VALUES (1, '2027-01-02')`

// A validated CHECK of another expression holds the name the not-null
// helper derives. It is not the helper: taking it as the proof would send
// SET NOT NULL into a scan under ACCESS EXCLUSIVE, and the cleanup would drop
// a CHECK the step did not create. The step refuses it on both paths and
// leaves it, and the column, as they were.
func TestEnsureAttachConstraintsRefusesAnotherCheckUnderTheHelpersName(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		t.Run("before SET NOT NULL", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, eventsParent)
			table := f.attachTable(t, "events_2027")
			helper := table.proof(attachProofNotNull, "(id IS NOT NULL)", "")
			f.prove(t, "events_2027", helper.name(), "day IS NOT NULL")

			_, err := ensureAttachConstraints(t.Context(), f.pool, f.tableOID(t, "events"), table, attachProofTestBudget, 0, nil, nil)
			require.ErrorIs(t, err, errAttachProofNameTaken)
			assert.False(t, f.notNull(t, "events_2027", "id"), "SET NOT NULL never ran")
			assert.Equal(t, map[string]bool{helper.name(): true}, f.checks(t, "events_2027"), "the other CHECK stays")
		})

		t.Run("in the cleanup of a column already NOT NULL", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, eventsParent)
			f.exec(t, `ALTER TABLE %[1]s.events_2027 ALTER COLUMN id SET NOT NULL`)
			table := f.attachTable(t, "events_2027")
			helper := table.proof(attachProofNotNull, "(id IS NOT NULL)", "")
			f.prove(t, "events_2027", helper.name(), "day IS NOT NULL")

			_, err := ensureAttachConstraints(t.Context(), f.pool, f.tableOID(t, "events"), table, attachProofTestBudget, 0, nil, nil)
			require.ErrorIs(t, err, errAttachProofNameTaken)
			assert.Equal(t, map[string]bool{helper.name(): true}, f.checks(t, "events_2027"), "the other CHECK is not dropped")
		})
	})
}

// The helper's expression names the column as the server prints it, quoted
// where the name needs it, so the step finds the helper it added and SET NOT
// NULL goes through.
func TestEnsureAttachConstraintsSetsAColumnWhoseNameNeedsQuoting(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, `
			CREATE TABLE %[1]s.events (
				"Event Id" bigint NOT NULL,
				day date
			) PARTITION BY RANGE (day);
			CREATE TABLE %[1]s.events_2027 (
				"Event Id" bigint,
				day date
			)`)
		table := f.attachTable(t, "events_2027")

		results, err := ensureAttachConstraints(t.Context(), f.pool, f.tableOID(t, "events"), table, attachProofTestBudget, 0, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, []attachConstraintResult{{column: "Event Id", action: attachConstraintSetNotNull}}, results)
		assert.True(t, f.notNull(t, "events_2027", "Event Id"))
		assert.Empty(t, f.checks(t, "events_2027"), "the helper is dropped")
	})
}

// A validated CHECK under the parent's name, with the parent's expression,
// is NO INHERIT. The attach will not merge it with the parent's, so the step
// does not report it found.
func TestEnsureAttachConstraintsRefusesANoInheritCheckOfTheParentsName(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, paymentsParent)
		f.exec(t, `ALTER TABLE %[1]s.payments_2027
			ADD CONSTRAINT payments_amount_positive CHECK (amount > 0) NO INHERIT`)

		_, err := ensureAttachConstraints(t.Context(), f.pool, f.tableOID(t, "payments"), f.attachTable(t, "payments_2027"),
			attachProofTestBudget, 0, nil, nil)
		require.ErrorIs(t, err, errAttachProofNameTaken)
	})
}

// A NOT VALID CHECK under the parent's name, with the parent's expression,
// is the parent's CHECK waiting for its validation: the step validates it
// and reports so.
func TestEnsureAttachConstraintsValidatesANotValidCheckOfTheParentsName(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, paymentsParent)
		f.exec(t, `ALTER TABLE %[1]s.payments_2027
			ADD CONSTRAINT payments_amount_positive CHECK (amount > 0) NOT VALID`)

		results, err := ensureAttachConstraints(t.Context(), f.pool, f.tableOID(t, "payments"), f.attachTable(t, "payments_2027"),
			attachProofTestBudget, 0, nil, nil)
		require.NoError(t, err)
		require.NotEmpty(t, results)
		assert.Equal(t, attachConstraintResult{check: "payments_amount_positive", action: attachConstraintValidated}, results[0])
		assert.True(t, f.checks(t, "payments_2027")["payments_amount_positive"])
	})
}

// For a column of a composite type, IS NOT NULL is the row test, true only
// when every field is non-null. A helper CHECK would refuse the row (1,
// NULL), which the column's NOT NULL accepts, and would not let SET NOT NULL
// skip its scan, so the step refuses the column before adding anything.
func TestEnsureAttachConstraintsRefusesACompositeNotNullColumn(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, `
			CREATE TYPE %[1]s.pair AS (
				a int,
				b int
			);
			CREATE TABLE %[1]s.events (
				id bigint NOT NULL,
				p %[1]s.pair NOT NULL,
				day date
			) PARTITION BY RANGE (day);
			CREATE TABLE %[1]s.events_2027 (
				id bigint NOT NULL,
				p %[1]s.pair,
				day date
			);
			INSERT INTO %[1]s.events_2027 VALUES (1, ROW(1, NULL)::%[1]s.pair, '2027-01-02')`)

		_, err := ensureAttachConstraints(t.Context(), f.pool, f.tableOID(t, "events"), f.attachTable(t, "events_2027"),
			attachProofTestBudget, 0, nil, nil)
		require.ErrorIs(t, err, errAttachColumnRowType)
		assert.Empty(t, f.checks(t, "events_2027"), "nothing was added")
		assert.False(t, f.notNull(t, "events_2027", "p"))
	})
}

// On PostgreSQL 18 a not-null constraint can be NOT VALID. SET NOT NULL
// would validate it under ACCESS EXCLUSIVE, so the step refuses the column
// and adds nothing.
func TestEnsureAttachConstraintsRefusesANotValidNotNull(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		var version int
		require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT current_setting('server_version_num')::int`).Scan(&version))
		if version < 180000 {
			t.Skip("a NOT VALID not-null constraint needs PostgreSQL 18")
		}
		f.exec(t, eventsParent)
		f.exec(t, `ALTER TABLE %[1]s.events_2027
			ADD CONSTRAINT events_2027_id_not_null NOT NULL id NOT VALID`)

		_, err := ensureAttachConstraints(t.Context(), f.pool, f.tableOID(t, "events"), f.attachTable(t, "events_2027"),
			attachProofTestBudget, 0, nil, nil)
		require.ErrorIs(t, err, errAttachNotNullNotValid)
		assert.Empty(t, f.checks(t, "events_2027"), "nothing was added")
	})
}

// A parent dropped since the run resolved it is not a parent with no
// constraints: the step refuses it rather than return an empty result.
func TestEnsureAttachConstraintsRefusesAParentThatIsGone(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, paymentsParent)
		parent, table := f.tableOID(t, "payments"), f.attachTable(t, "payments_2027")
		f.exec(t, `DROP TABLE %[1]s.payments`)

		_, err := ensureAttachConstraints(t.Context(), f.pool, parent, table, attachProofTestBudget, 0, nil, nil)
		require.ErrorIs(t, err, ErrInvariantViolation)
		assert.Empty(t, f.checks(t, "payments_2027"), "nothing was added")
	})
}

// The parent gains a CHECK while the step runs. The step finishes what it
// read, then finds the parent changed and says so; the rerun gives the table
// the new CHECK too, and the attach goes through.
func TestEnsureAttachConstraintsReportsAParentThatChangedWhileItRan(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, eventsParent)
		parent, table := f.tableOID(t, "events"), f.attachTable(t, "events_2027")
		var once sync.Once
		results, err := ensureAttachConstraints(t.Context(), f.pool, parent, table, attachProofTestBudget, 0, nil,
			func(progress.Operation, string) {
				once.Do(func() {
					f.exec(t, `ALTER TABLE %[1]s.events ADD CONSTRAINT events_id_positive CHECK (id > 0)`)
				})
			})
		require.ErrorIs(t, err, errAttachParentChanged)
		assert.Equal(t, []attachConstraintResult{{column: "id", action: attachConstraintSetNotNull}}, results,
			"what the step read is finished")

		results, err = ensureAttachConstraints(t.Context(), f.pool, parent, table, attachProofTestBudget, 0, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, []attachConstraintResult{
			{check: "events_id_positive", action: attachConstraintAdded},
			{column: "id", action: attachConstraintFound},
		}, results)
		f.attach(t, `ALTER TABLE %[1]s.events
			ATTACH PARTITION %[1]s.events_2027
			FOR VALUES FROM ('2027-01-01') TO ('2028-01-01')`)
	})
}

// The helper CHECK is gone when SET NOT NULL's transaction takes the table's
// lock. Deciding again under the lock, the step refuses to run the statement,
// which would scan, and the column stays nullable.
func TestSetAttachNotNullRefusesAMissingHelper(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, eventsParent)
		table := f.attachTable(t, "events_2027")
		helper := table.proof(attachProofNotNull, "(id IS NOT NULL)", "")
		setSQL := "ALTER TABLE " + pgx.Identifier{f.schema, "events_2027"}.Sanitize() + ` ALTER COLUMN "id" SET NOT NULL`

		err := setAttachNotNull(t.Context(), briefTx{pool: f.pool}, helper, "id", setSQL)
		require.ErrorIs(t, err, ErrInvariantViolation)
		assert.False(t, f.notNull(t, "events_2027", "id"))
	})
}

// A reader holds the table for a second, five times the SET NOT NULL's
// lock_timeout, after the helper is validated. With a holder wait the step
// waits outside the lock queue until the reader ends, then sets the column
// NOT NULL.
func TestEnsureAttachConstraintsWaitsOutALongReaderBeforeSetNotNull(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, eventsParent)
		table := f.attachTable(t, "events_2027")
		helper := table.proof(attachProofNotNull, "(id IS NOT NULL)", "")
		f.prove(t, "events_2027", helper.name(), helper.expr)
		pool := f.poolWithLockTimeout(t, 200*time.Millisecond)
		release, _ := f.holdLock(t, "events_2027", "ACCESS SHARE")
		var wg sync.WaitGroup
		defer wg.Wait()
		wg.Go(func() {
			// The reader's transaction: held for a second, then ended.
			time.Sleep(time.Second)
			release()
		})

		results, err := ensureAttachConstraints(t.Context(), pool, f.tableOID(t, "events"), table, attachProofTestBudget, 10*time.Second, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, []attachConstraintResult{{column: "id", action: attachConstraintSetNotNull}}, results)
		assert.True(t, f.notNull(t, "events_2027", "id"))
	})
}
