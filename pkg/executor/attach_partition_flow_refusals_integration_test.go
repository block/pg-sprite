package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// These tests cover the online attach's refusals that need the catalog, and
// the parents and failures the end-to-end tests do not reach. Every refusal
// here is read before anything is added to the table.

// requireNothingAdded asserts the table got no proof, no constraint, and no
// index.
func (f attachProofFixture) requireNothingAdded(t *testing.T, table string) {
	t.Helper()
	assert.Empty(t, f.partitionBound(t, table))
	assert.Empty(t, f.constraintNames(t, table))
	var indexes int
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT count(*) FROM pg_index WHERE indrelid = $1::regclass",
		pgx.Identifier{f.schema, table}.Sanitize()).Scan(&indexes))
	assert.Zero(t, indexes)
}

// A table with a column the parent lacks is one PostgreSQL's ATTACH
// refuses; the run refuses it before it gives the table anything.
func TestAttachPartitionOnlineRefusesAColumnTheParentLacks(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, ordersAttachParent)
		f.exec(t, `ALTER TABLE %[1]s.orders_2027 ADD COLUMN note text`)

		rep, err := f.attachOnline(t, f.pool, attachOrders2027Online, attachFlowOptions, RunStep{})
		require.ErrorIs(t, err, ErrAttachPartitionTable)
		assert.Equal(t, CodeAttachPartitionTable, OutcomeCode(err))
		assert.Empty(t, rep.Steps)
		f.requireNothingAdded(t, "orders_2027")
	})
}

// A column of another type than the parent's is one PostgreSQL's ATTACH
// refuses too: total is bigint here and int on the parent.
func TestAttachPartitionOnlineRefusesAColumnOfAnotherType(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, ordersAttachParent)
		f.exec(t, `ALTER TABLE %[1]s.orders_2027 ALTER COLUMN total TYPE bigint`)

		_, err := f.attachOnline(t, f.pool, attachOrders2027Online, attachFlowOptions, RunStep{})
		require.ErrorIs(t, err, ErrAttachPartitionTable)
		f.requireNothingAdded(t, "orders_2027")
	})
}

// A DEFAULT partition that is itself partitioned cannot carry the proof the
// flow validates, so the run refuses before it gives the table anything.
func TestAttachPartitionOnlineRefusesAPartitionedDefaultPartition(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, `
			CREATE TABLE %[1]s.orders (
				id bigint NOT NULL,
				day date NOT NULL,
				region text NOT NULL,
				CONSTRAINT orders_pkey PRIMARY KEY (id, day, region)
			) PARTITION BY RANGE (day);
			CREATE TABLE %[1]s.orders_default PARTITION OF %[1]s.orders DEFAULT
				PARTITION BY LIST (region);
			CREATE TABLE %[1]s.orders_2027 (
				id bigint,
				day date,
				region text
			)`)

		_, err := f.attachOnline(t, f.pool, attachOrders2027Online, attachFlowOptions, RunStep{})
		require.ErrorIs(t, err, ErrAttachPartitionTable)
		f.requireNothingAdded(t, "orders_2027")
	})
}

// A pool that cannot hold a concurrent index build's sessions is refused
// before the lock is taken, not when the index step reaches its first
// build.
func TestAttachPartitionOnlineRefusesAPoolTooSmallForTheIndexBuilds(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, ordersAttachParent)
		cfg, err := pgxpool.ParseConfig(f.url)
		require.NoError(t, err)
		cfg.MaxConns = 1
		small, err := pgxpool.NewWithConfig(t.Context(), cfg)
		require.NoError(t, err)
		t.Cleanup(small.Close)

		_, err = f.attachOnline(t, small, attachOrders2027Online, attachFlowOptions, RunStep{})
		require.ErrorIs(t, err, ErrPoolTooSmall)
		assert.Equal(t, CodePoolTooSmall, OutcomeCode(err))
		f.requireNothingAdded(t, "orders_2027")
	})
}

// The run holds the parent's advisory table lock (LK-1), so while another
// session holds it the run is refused before it reads anything.
func TestAttachPartitionOnlineRefusesASecondRunOnTheSameParent(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, ordersAttachParent)
		lock, err := dbconn.AcquireTableLock(t.Context(), dbconn.Config{URL: f.url}, f.schema, "orders")
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, lock.Release(context.WithoutCancel(t.Context()))) })

		_, err = f.attachOnline(t, f.pool, attachOrders2027Online, attachFlowOptions, RunStep{})
		_, held := errors.AsType[*dbconn.TableLockHeldError](err)
		assert.True(t, held, "%v", err)
		f.requireNothingAdded(t, "orders_2027")
	})
}

// A list-partitioned parent with no DEFAULT partition needs no DEFAULT
// proof and no write window: the run attaches without either.
func TestAttachPartitionOnlineAttachesAListPartitionUnderAParentWithoutADefault(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, `
			CREATE TABLE %[1]s.events (
				id bigint NOT NULL,
				region text NOT NULL,
				CONSTRAINT events_pkey PRIMARY KEY (id, region)
			) PARTITION BY LIST (region);
			CREATE TABLE %[1]s.events_eu PARTITION OF %[1]s.events
				FOR VALUES IN ('eu');
			CREATE TABLE %[1]s.events_us (
				id bigint,
				region text
			);
			INSERT INTO %[1]s.events_us VALUES (1, 'us')`)
		o := attachFlowOptions
		o.AcceptDefaultWriteWindow = false

		rep, err := f.attachOnlineTo(t, "events", f.pool, `ALTER TABLE %[1]s.events
			ATTACH PARTITION events_us
			FOR VALUES IN ('us')`, o, RunStep{})
		require.NoError(t, err)
		require.Len(t, rep.Steps, 4, "%+v", rep.Steps)
		assert.Equal(t, []AttachPartitionStep{
			{Kind: AttachPartitionStepBoundProof, Name: rep.Steps[0].Name, Action: "added"},
			{Kind: AttachPartitionStepNotNull, Name: "id", Action: "set-not-null"},
			{Kind: AttachPartitionStepNotNull, Name: "region", Action: "set-not-null"},
			{Kind: AttachPartitionStepIndex, Name: "events_pkey", Action: "keyed"},
		}, rep.Steps)
		assert.True(t, rep.Attached)
		assert.Equal(t, "FOR VALUES IN ('us')", rep.Bound)
		assert.Empty(t, f.proofConstraints(t, "events_us"))
	})
}

// Rows of the DEFAULT partition inside the new bound break its proof: the
// proof step drops it again, so the run reports no DEFAULT proof at all,
// and the table keeps what it was given and stays unattached.
func TestAttachPartitionOnlineStopsAtADefaultRowInsideTheBound(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, ordersAttachParent)
		f.exec(t, `INSERT INTO %[1]s.orders VALUES (8, '2027-05-01', 'eu', 1, 10)`)

		rep, err := f.attachOnline(t, f.pool, attachOrders2027Online, attachFlowOptions, RunStep{})
		require.ErrorIs(t, err, errAttachProofViolation)
		require.Len(t, rep.Steps, 8, "%+v", rep.Steps)
		assert.Equal(t, AttachPartitionStepForeignKey, rep.Steps[7].Kind)
		assert.Empty(t, f.partitionBound(t, "orders_2027"))
		assert.Empty(t, f.proofConstraints(t, "orders_default"))
	})
}
