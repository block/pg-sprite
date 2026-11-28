package executor

import (
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/progress"
)

// These tests run the online attach end to end against the server: a table
// that lacks everything the parent requires is given it while writes go on,
// and the ATTACH that follows scans and builds nothing.

var attachFlowOptions = AttachPartitionOptions{
	Attach:                   Budget{LockTimeout: time.Second, StatementTimeout: 5 * time.Second},
	Validate:                 ValidateBudget{LockTimeout: 5 * time.Second, Overall: time.Minute},
	Index:                    ConcurrentBudget{Overall: time.Minute},
	HolderWait:               time.Second,
	AcceptDefaultWriteWindow: true,
}

// ordersAttachParent is a range-partitioned table with a primary key, an
// index, a CHECK, a NOT NULL column outside the key, and a foreign key, and a
// DEFAULT partition holding a row outside the 2027 bound. orders_2027 has
// the same columns, every one nullable, none of the parent's indexes or
// constraints, and rows inside the bound.
const ordersAttachParent = `
	CREATE TABLE %[1]s.customers (
		id bigint PRIMARY KEY
	);
	INSERT INTO %[1]s.customers VALUES (10), (11);
	CREATE TABLE %[1]s.orders (
		id bigint NOT NULL,
		day date NOT NULL,
		region text NOT NULL,
		total int,
		customer_id bigint,
		CONSTRAINT orders_pkey PRIMARY KEY (id, day),
		CONSTRAINT orders_total_check CHECK (total >= 0),
		CONSTRAINT orders_customer_fkey FOREIGN KEY (customer_id) REFERENCES %[1]s.customers (id)
	) PARTITION BY RANGE (day);
	CREATE INDEX orders_region_idx ON %[1]s.orders (region);
	CREATE TABLE %[1]s.orders_2026 PARTITION OF %[1]s.orders
		FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
	CREATE TABLE %[1]s.orders_default PARTITION OF %[1]s.orders DEFAULT;
	INSERT INTO %[1]s.orders VALUES (1, '2030-06-01', 'eu', 5, 10);
	CREATE TABLE %[1]s.orders_2027 (
		id bigint,
		day date,
		region text,
		total int,
		customer_id bigint
	);
	INSERT INTO %[1]s.orders_2027 VALUES
		(2, '2027-03-01', 'eu', 7, 10),
		(3, '2027-09-01', 'us', 0, 11)`

const attachOrders2027Online = `ALTER TABLE %[1]s.orders
	ATTACH PARTITION orders_2027
	FOR VALUES FROM ('2027-01-01') TO ('2028-01-01')`

const orders2027Bound = "FOR VALUES FROM ('2027-01-01') TO ('2028-01-01')"

// attachOnline runs the online attach of the statement, with %[1]s standing
// for the fixture's schema, on pool, preflighting the orders table.
func (f attachProofFixture) attachOnline(t *testing.T, pool *pgxpool.Pool, sql string, o AttachPartitionOptions, at RunStep) (AttachPartitionReport, error) {
	t.Helper()
	pt, err := preflight.CheckTable(t.Context(), f.pool, f.schema, "orders", preflight.NoSizeLimit)
	require.NoError(t, err)
	return AttachPartitionOnlineAsStep(t.Context(), dbconn.Config{URL: f.url}, pool, pt, fmt.Sprintf(sql, f.schema), o, at)
}

// constraintNames lists the table's constraints, with whether each is
// attached to a constraint of a parent.
func (f attachProofFixture) constraintNames(t *testing.T, table string) map[string]bool {
	t.Helper()
	rows, err := f.pool.Query(t.Context(), `SELECT conname, conparentid <> 0 FROM pg_catalog.pg_constraint
		WHERE conrelid = $1::regclass AND contype <> 'n'`, pgx.Identifier{f.schema, table}.Sanitize())
	require.NoError(t, err)
	got := map[string]bool{}
	var name string
	var attached bool
	_, err = pgx.ForEachRow(rows, []any{&name, &attached}, func() error {
		got[name] = attached
		return nil
	})
	require.NoError(t, err)
	return got
}

// From a table that lacks every index and constraint of the parent, the run
// gives it each one, proves the bound on it and its exclusion on the DEFAULT
// partition, and attaches it without the ATTACH scanning either table. A
// rerun finds the partition and gives it nothing.
func TestAttachPartitionOnlineGivesTheTableWhatTheParentRequiresAndAttachesWithoutAScan(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, ordersAttachParent)
		pool, notices := f.debugPool(t)
		tracker, err := progress.NewTracker(progress.WallClock{})
		require.NoError(t, err)
		tracker.Start(1, progress.OperationBrief)

		rep, err := f.attachOnline(t, pool, attachOrders2027Online, attachFlowOptions, RunStep{Tracker: tracker, Step: 1})
		require.NoError(t, err)
		require.Len(t, rep.Steps, 9, "%+v", rep.Steps)
		assert.Equal(t, AttachPartitionStep{Kind: AttachPartitionStepBoundProof, Name: rep.Steps[0].Name, Action: "added"}, rep.Steps[0])
		assert.Equal(t, []AttachPartitionStep{
			{Kind: AttachPartitionStepCheck, Name: "orders_total_check", Action: "added"},
			{Kind: AttachPartitionStepNotNull, Name: "id", Action: "set-not-null"},
			{Kind: AttachPartitionStepNotNull, Name: "day", Action: "set-not-null"},
			{Kind: AttachPartitionStepNotNull, Name: "region", Action: "set-not-null"},
			{Kind: AttachPartitionStepIndex, Name: "orders_pkey", Action: "keyed"},
			{Kind: AttachPartitionStepIndex, Name: "orders_region_idx", Action: "built"},
			{Kind: AttachPartitionStepForeignKey, Name: "orders_customer_fkey", Action: "added"},
		}, rep.Steps[1:8])
		assert.Equal(t, AttachPartitionStepDefaultProof, rep.Steps[8].Kind)
		assert.Equal(t, "added", rep.Steps[8].Action)
		assert.True(t, rep.Attached)
		assert.Equal(t, orders2027Bound, rep.Bound)
		assert.Equal(t, "orders_2027", rep.Table)
		// The validations before the attach each report the table they
		// verify; the DEFAULT partition's proof is the last of them. What the
		// server reports after it is the ATTACH, which scans neither table.
		got := notices()
		last := lastIndex(got, `verifying table "orders_default"`)
		require.GreaterOrEqual(t, last, 0, "the DEFAULT partition's proof was validated: %q", got)
		assert.Equal(t, []string{
			`partition constraint for table "orders_2027" is implied by existing constraints`,
			`updated partition constraint for default partition "orders_default" is implied by existing constraints`,
		}, got[last+1:])
		assert.Equal(t, orders2027Bound, f.partitionBound(t, "orders_2027"))
		assert.Empty(t, f.proofConstraints(t, "orders_2027"))
		assert.Empty(t, f.proofConstraints(t, "orders_default"))
		// The key and the foreign key the table carries are the parent's,
		// adopted; a CHECK is inherited by name, not attached.
		assert.Equal(t, map[string]bool{
			"orders_total_check": false,
			fmt.Sprintf("orders_pkey_p%d", f.parentOIDOf(t, "orders_2027")):          true,
			fmt.Sprintf("orders_customer_fkey_p%d", f.parentOIDOf(t, "orders_2027")): true,
		}, f.constraintNames(t, "orders_2027"))

		again, err := f.attachOnline(t, f.pool, attachOrders2027Online, attachFlowOptions, RunStep{})
		require.NoError(t, err)
		assert.False(t, again.Attached)
		assert.Equal(t, orders2027Bound, again.Bound)
		assert.Empty(t, again.Steps)
	})
}

// A parent with a DEFAULT partition is refused, before anything is added,
// unless the caller accepts that the DEFAULT partition's proof refuses
// writes inside the new bound until the attach commits.
func TestAttachPartitionOnlineRefusesADefaultPartitionWithoutTheWriteWindow(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, ordersAttachParent)
		o := attachFlowOptions
		o.AcceptDefaultWriteWindow = false

		rep, err := f.attachOnline(t, f.pool, attachOrders2027Online, o, RunStep{})
		require.ErrorIs(t, err, ErrAttachPartitionDefaultWindow)
		assert.Empty(t, rep.Steps)
		assert.Empty(t, f.partitionBound(t, "orders_2027"))
		assert.Empty(t, f.constraintNames(t, "orders_2027"))
		var indexes int
		require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT count(*) FROM pg_index WHERE indrelid = $1::regclass",
			pgx.Identifier{f.schema, "orders_2027"}.Sanitize()).Scan(&indexes))
		assert.Zero(t, indexes)
	})
}

// A row outside the bound fails the table's proof before anything else is
// given to the table: the proof stays NOT VALID, enforced for new rows, and
// the table is not attached.
func TestAttachPartitionOnlineStopsAtARowOutsideTheBound(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, ordersAttachParent)
		f.exec(t, `INSERT INTO %[1]s.orders_2027 VALUES (4, '2029-01-01', 'eu', 1, 10)`)

		rep, err := f.attachOnline(t, f.pool, attachOrders2027Online, attachFlowOptions, RunStep{})
		require.ErrorIs(t, err, errAttachProofViolation)
		assert.Empty(t, rep.Steps)
		assert.Empty(t, f.partitionBound(t, "orders_2027"))
		assert.Len(t, f.proofConstraints(t, "orders_2027"), 1)
	})
}

// The run takes a plain table only: a partitioned table, which the attach
// would make a sub-partition, is refused before anything is added.
func TestAttachPartitionOnlineRefusesAPartitionedTable(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, ordersAttachParent)
		f.exec(t, `
			CREATE TABLE %[1]s.orders_2028 (
				id bigint,
				day date,
				region text,
				total int,
				customer_id bigint
			) PARTITION BY LIST (region)`)

		_, err := f.attachOnline(t, f.pool, `ALTER TABLE %[1]s.orders
			ATTACH PARTITION orders_2028
			FOR VALUES FROM ('2028-01-01') TO ('2029-01-01')`, attachFlowOptions, RunStep{})
		require.ErrorIs(t, err, ErrAttachPartitionTable)
		assert.Empty(t, f.proofConstraints(t, "orders_2028"))
	})
}

// A table that is already a partition of another parent is refused before
// anything is added to it: it is the caller's table, not a run's state gone
// wrong.
func TestAttachPartitionOnlineRefusesAPartitionOfAnotherParent(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, ordersAttachParent)
		f.exec(t, `
			CREATE TABLE %[1]s.archive (
				id bigint,
				day date,
				region text,
				total int,
				customer_id bigint
			) PARTITION BY RANGE (day);
			ALTER TABLE %[1]s.archive ATTACH PARTITION %[1]s.orders_2027
				FOR VALUES FROM ('2027-01-01') TO ('2028-01-01')`)

		rep, err := f.attachOnline(t, f.pool, attachOrders2027Online, attachFlowOptions, RunStep{})
		require.ErrorIs(t, err, ErrAttachPartitionTable)
		assert.Empty(t, rep.Steps)
		assert.Empty(t, f.proofConstraints(t, "orders_2027"))
	})
}

// Attaching a DEFAULT partition needs a proof against every other
// partition's bound, which the run does not derive: it is refused before
// the run takes the lock.
func TestAttachPartitionOnlineRefusesADefaultBound(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, ordersAttachParent)
		f.exec(t, `ALTER TABLE %[1]s.orders DETACH PARTITION %[1]s.orders_default`)

		_, err := f.attachOnline(t, f.pool, `ALTER TABLE %[1]s.orders
			ATTACH PARTITION orders_default DEFAULT`, attachFlowOptions, RunStep{})
		require.ErrorIs(t, err, ErrAttachPartitionUnsupportedShape)
	})
}

// lastIndex is the index of the last notice equal to want, -1 for none.
func lastIndex(notices []string, want string) int {
	for i := len(notices) - 1; i >= 0; i-- {
		if notices[i] == want {
			return i
		}
	}
	return -1
}
