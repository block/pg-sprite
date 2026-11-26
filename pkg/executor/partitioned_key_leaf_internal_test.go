package executor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
)

// leafKeyBudget bounds the step's concurrent builds in these tests.
var leafKeyBudget = ConcurrentBudget{Overall: time.Minute}

// leafKeyFixture is one subtest's view of a shared server: the engine's
// bounded pool and a schema of its own.
type leafKeyFixture struct {
	pool   *pgxpool.Pool
	schema string
}

// withLeafKeyServer runs the subtests against one server, each in a schema of
// its own, so the container start is paid once per test.
func withLeafKeyServer(t *testing.T, run func(t *testing.T, newFixture func(t *testing.T) leafKeyFixture)) {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	run(t, func(t *testing.T) leafKeyFixture {
		t.Helper()
		return leafKeyFixture{pool: pool, schema: testutil.NewSchema(t, pool)}
	})
}

// exec runs sql with every %[1]s replaced by the fixture's schema.
func (f leafKeyFixture) exec(t *testing.T, sql string) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), fmt.Sprintf(sql, f.schema))
	require.NoError(t, err)
}

// events creates the range-partitioned table every test starts from: two
// monthly leaves holding rows, and a primary key candidate (id, created_at)
// whose columns are NOT NULL on the parent and so on every leaf.
func (f leafKeyFixture) events(t *testing.T) {
	t.Helper()
	f.exec(t, `
		CREATE TABLE %[1]s.events (
		    id         bigint      NOT NULL,
		    created_at timestamptz NOT NULL,
		    payload    text
		) PARTITION BY RANGE (created_at)`)
	f.exec(t, `
		CREATE TABLE %[1]s.events_2026_01 PARTITION OF %[1]s.events
		    FOR VALUES FROM ('2026-01-01') TO ('2026-02-01')`)
	f.exec(t, `
		CREATE TABLE %[1]s.events_2026_02 PARTITION OF %[1]s.events
		    FOR VALUES FROM ('2026-02-01') TO ('2026-03-01')`)
	f.exec(t, `
		INSERT INTO %[1]s.events (id, created_at, payload)
		SELECT g, '2026-01-01'::timestamptz + mod(g, 59) * interval '1 day', 'p' || g
		  FROM generate_series(1, 2000) g`)
}

// leaf returns the partition as the tree listing reads it, the input the step
// takes.
func (f leafKeyFixture) leaf(t *testing.T, parent, name string) partitionNode {
	t.Helper()
	tree, err := listPartitionTree(t.Context(), f.pool, f.oid(t, parent))
	require.NoError(t, err)
	for _, n := range tree {
		if n.name == name {
			return n
		}
	}
	require.FailNow(t, "partition not listed", "%s under %s", name, parent)
	return partitionNode{}
}

// serverMajor is the major version of the server the fixture runs on.
func (f leafKeyFixture) serverMajor(t *testing.T) int {
	t.Helper()
	major, err := dbconn.ServerMajor(t.Context(), f.pool)
	require.NoError(t, err)
	return major
}

// cancelDetach leaves the partition detach-pending: a reader of the parent
// holds DETACH PARTITION ... CONCURRENTLY in its wait for older transactions,
// which its statement_timeout then cancels after the first transaction has
// committed.
func (f leafKeyFixture) cancelDetach(t *testing.T, parent, partition string) {
	t.Helper()
	holder, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	_, err = holder.Exec(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{f.schema, parent}.Sanitize())
	require.NoError(t, err)
	conn, err := f.pool.Acquire(t.Context())
	require.NoError(t, err)
	_, err = conn.Exec(t.Context(), "SET statement_timeout = '700ms'")
	require.NoError(t, err)
	_, err = conn.Exec(t.Context(), "ALTER TABLE "+pgx.Identifier{f.schema, parent}.Sanitize()+
		" DETACH PARTITION "+pgx.Identifier{f.schema, partition}.Sanitize()+" CONCURRENTLY")
	require.Error(t, err, "the detach waits on the reader and is cancelled")
	_, err = conn.Exec(t.Context(), "RESET statement_timeout")
	require.NoError(t, err)
	conn.Release()
	require.NoError(t, holder.Rollback(t.Context()))
	var pending bool
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT inhdetachpending
		  FROM pg_inherits
		 WHERE inhrelid = $1::regclass`, pgx.Identifier{f.schema, partition}.Sanitize()).Scan(&pending))
	require.True(t, pending, "the cancelled detach left the partition detach-pending")
}

func (f leafKeyFixture) oid(t *testing.T, relation string) uint32 {
	t.Helper()
	var oid uint32
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT $1::regclass::oid`,
		pgx.Identifier{f.schema, relation}.Sanitize()).Scan(&oid))
	return oid
}

// keyConstraint is one PRIMARY KEY or UNIQUE constraint of a table, with the
// index behind it, as the catalog reads it.
type keyConstraint struct {
	oid        uint32
	contype    string
	parentOID  uint32
	index      string
	indexOID   uint32
	indexValid bool
}

// keyConstraints lists a table's PRIMARY KEY and UNIQUE constraints by name.
func (f leafKeyFixture) keyConstraints(t *testing.T, table string) map[string]keyConstraint {
	t.Helper()
	rows, err := f.pool.Query(t.Context(), `
		SELECT k.conname, k.oid, k.contype::text, k.conparentid, c.relname, c.oid, i.indisvalid
		  FROM pg_constraint k
		  JOIN pg_class c ON c.oid = k.conindid
		  JOIN pg_index i ON i.indexrelid = k.conindid
		 WHERE k.conrelid = $1::regclass
		   AND k.contype IN ('p', 'u')`, pgx.Identifier{f.schema, table}.Sanitize())
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]keyConstraint{}
	for rows.Next() {
		var name string
		var k keyConstraint
		require.NoError(t, rows.Scan(&name, &k.oid, &k.contype, &k.parentOID, &k.index, &k.indexOID, &k.indexValid))
		out[name] = k
	}
	require.NoError(t, rows.Err())
	return out
}

// indexes lists a table's indexes by name with their OIDs and validity.
func (f leafKeyFixture) indexes(t *testing.T, table string) map[string]keyConstraint {
	t.Helper()
	rows, err := f.pool.Query(t.Context(), `
		SELECT c.relname, c.oid, i.indisvalid
		  FROM pg_index i
		  JOIN pg_class c ON c.oid = i.indexrelid
		 WHERE i.indrelid = $1::regclass`, pgx.Identifier{f.schema, table}.Sanitize())
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]keyConstraint{}
	for rows.Next() {
		var name string
		var k keyConstraint
		require.NoError(t, rows.Scan(&name, &k.indexOID, &k.indexValid))
		k.index = name
		out[name] = k
	}
	require.NoError(t, rows.Err())
	return out
}

// eventsPrimaryKey is ALTER TABLE events ADD CONSTRAINT events_pkey
// PRIMARY KEY (id, created_at), as the step takes it.
var eventsPrimaryKey = partitionedKeySpec{kind: partitionedPrimaryKey, name: "events_pkey", columns: []string{"id", "created_at"}}

// eventsUniqueKey is ALTER TABLE events ADD CONSTRAINT events_id_key
// UNIQUE (id, created_at), as the step takes it.
var eventsUniqueKey = partitionedKeySpec{kind: partitionedUniqueKey, name: "events_id_key", columns: []string{"id", "created_at"}}

// derivedKeyName is the name the step gives a leaf's index and constraint.
func derivedKeyName(spec partitionedKeySpec, leaf partitionNode) string {
	return fmt.Sprintf("%s_p%d", spec.name, leaf.oid)
}

// The step resumes from whatever the leaf holds: it builds what is missing,
// finishes what an earlier call left, and changes nothing once the leaf has
// the key.
func TestEnsureLeafKeyResume(t *testing.T) {
	withLeafKeyServer(t, func(t *testing.T, newFixture func(t *testing.T) leafKeyFixture) {
		// A fresh leaf gets the unique index built concurrently under the
		// derived name, then a primary key constraint on it under the same
		// name, so USING INDEX renames nothing. The constraint is attached
		// to no parent constraint yet. A rerun finds it and changes nothing.
		t.Run("fresh build then rerun is a no-op", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			leaf := f.leaf(t, "events", "events_2026_01")
			name := derivedKeyName(eventsPrimaryKey, leaf)

			res, err := ensureLeafKey(t.Context(), f.pool, eventsPrimaryKey, leaf, leafKeyBudget)

			require.NoError(t, err)
			assert.Equal(t, leafKeyBuilt, res.action)
			assert.Equal(t, name, res.constraint)
			assert.Equal(t, name, res.index)
			assert.False(t, res.recoveredDebris)
			got := f.keyConstraints(t, "events_2026_01")
			require.Len(t, got, 1)
			assert.Equal(t, keyConstraint{
				oid: res.constraintOID, contype: "p", parentOID: 0,
				index: name, indexOID: res.indexOID, indexValid: true,
			}, got[name])

			again, err := ensureLeafKey(t.Context(), f.pool, eventsPrimaryKey, leaf, leafKeyBudget)

			require.NoError(t, err)
			assert.Equal(t, leafKeyResult{
				action: leafKeyFound, constraint: name, constraintOID: res.constraintOID,
				index: name, indexOID: res.indexOID,
			}, again)
			assert.Equal(t, got, f.keyConstraints(t, "events_2026_01"), "the rerun changes nothing")
			assert.Len(t, f.indexes(t, "events_2026_01"), 1, "the rerun builds no second index")
		})

		// An earlier call that built the index and stopped before the
		// constraint left a valid unique index under the derived name. The
		// rerun only adds the constraint on it: the index keeps its OID.
		t.Run("constraint pending under the derived name", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			leaf := f.leaf(t, "events", "events_2026_01")
			name := derivedKeyName(eventsUniqueKey, leaf)
			f.exec(t, `CREATE UNIQUE INDEX `+name+` ON %[1]s.events_2026_01 (id, created_at)`)
			before := f.indexes(t, "events_2026_01")[name]

			res, err := ensureLeafKey(t.Context(), f.pool, eventsUniqueKey, leaf, leafKeyBudget)

			require.NoError(t, err)
			assert.Equal(t, leafKeyConstrained, res.action)
			assert.Equal(t, before.indexOID, res.indexOID, "the index is the one already there")
			got := f.keyConstraints(t, "events_2026_01")[name]
			assert.Equal(t, "u", got.contype)
			assert.Equal(t, before.indexOID, got.indexOID)
			assert.Zero(t, got.parentOID)
		})

		// A valid unique index of the key's definition under the user's own
		// name, attached to nothing, is given the constraint under that same
		// name: nothing is built and the user's index is not renamed.
		t.Run("constraint pending on a user index under its own name", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			leaf := f.leaf(t, "events", "events_2026_01")
			f.exec(t, `CREATE UNIQUE INDEX events_jan_id_created ON %[1]s.events_2026_01 (id, created_at)`)
			before := f.indexes(t, "events_2026_01")["events_jan_id_created"]

			res, err := ensureLeafKey(t.Context(), f.pool, eventsPrimaryKey, leaf, leafKeyBudget)

			require.NoError(t, err)
			assert.Equal(t, leafKeyResult{
				action: leafKeyConstrained, constraint: "events_jan_id_created", constraintOID: res.constraintOID,
				index: "events_jan_id_created", indexOID: before.indexOID,
			}, res)
			assert.Equal(t, []string{"events_jan_id_created"}, keys(f.indexes(t, "events_2026_01")))
			assert.Equal(t, "p", f.keyConstraints(t, "events_2026_01")["events_jan_id_created"].contype)
		})

		// A leaf that already has the requested primary key under a name of
		// its own is done: the step adopts it rather than add a second one.
		t.Run("adopts a matching user constraint under another name", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			leaf := f.leaf(t, "events", "events_2026_01")
			f.exec(t, `ALTER TABLE %[1]s.events_2026_01 ADD CONSTRAINT events_jan_key PRIMARY KEY (id, created_at)`)
			before := f.keyConstraints(t, "events_2026_01")

			res, err := ensureLeafKey(t.Context(), f.pool, eventsPrimaryKey, leaf, leafKeyBudget)

			require.NoError(t, err)
			assert.Equal(t, leafKeyFound, res.action)
			assert.Equal(t, "events_jan_key", res.constraint)
			assert.Equal(t, before["events_jan_key"].oid, res.constraintOID)
			assert.Equal(t, before, f.keyConstraints(t, "events_2026_01"), "nothing is added")
			assert.Len(t, f.indexes(t, "events_2026_01"), 1, "nothing is built")
		})

		// A unique build over duplicate rows fails with the server's unique
		// violation and leaves its invalid index under the derived name.
		// Once the rows are fixed, the rerun proves that index abandoned,
		// removes it, builds again, and adds the constraint.
		t.Run("debris of a failed build is recovered", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			f.exec(t, `INSERT INTO %[1]s.events (id, created_at) VALUES (1, '2026-01-01'), (1, '2026-01-01')`)
			leaf := f.leaf(t, "events", "events_2026_01")
			name := derivedKeyName(eventsPrimaryKey, leaf)

			_, err := ensureLeafKey(t.Context(), f.pool, eventsPrimaryKey, leaf, leafKeyBudget)

			var pgErr *pgconn.PgError
			require.ErrorAs(t, err, &pgErr)
			assert.Equal(t, "23505", pgErr.Code)
			assert.Equal(t, CodeDataFault, OutcomeCode(err),
				"the failed build reports the rows it failed on")
			assert.ErrorIs(t, err, ErrBuildLeftInvalidIndex,
				"the failed build reports the leftover it left, which the rerun recovers")
			debris := f.indexes(t, "events_2026_01")[name]
			assert.False(t, debris.indexValid, "the failed build leaves its invalid index")
			assert.Empty(t, f.keyConstraints(t, "events_2026_01"))

			f.exec(t, `DELETE FROM %[1]s.events_2026_01 WHERE id = 1 AND created_at = '2026-01-01' AND payload IS NULL`)

			res, err := ensureLeafKey(t.Context(), f.pool, eventsPrimaryKey, leaf, leafKeyBudget)

			require.NoError(t, err)
			assert.Equal(t, leafKeyBuilt, res.action)
			assert.True(t, res.recoveredDebris)
			assert.NotEqual(t, debris.indexOID, res.indexOID, "the debris is replaced, not reused")
			assert.Equal(t, []string{name}, keys(f.indexes(t, "events_2026_01")), "no debris remains")
			got := f.keyConstraints(t, "events_2026_01")[name]
			assert.True(t, got.indexValid)
			assert.Equal(t, res.indexOID, got.indexOID)
		})
	})
}

// The step refuses, typed, a leaf it cannot bring to the key without taking
// over or duplicating what is there, and leaves the leaf as it found it.
func TestEnsureLeafKeyRefusals(t *testing.T) {
	withLeafKeyServer(t, func(t *testing.T, newFixture func(t *testing.T) leafKeyFixture) {
		// A plain index on the key's columns under the derived name is not
		// the key's index: it is not unique. The step does not build under
		// another name and does not drop it.
		t.Run("mismatched index under the derived name", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			leaf := f.leaf(t, "events", "events_2026_01")
			name := derivedKeyName(eventsPrimaryKey, leaf)
			f.exec(t, `CREATE INDEX `+name+` ON %[1]s.events_2026_01 (id, created_at)`)
			before := f.indexes(t, "events_2026_01")

			_, err := ensureLeafKey(t.Context(), f.pool, eventsPrimaryKey, leaf, leafKeyBudget)

			require.ErrorIs(t, err, ErrPartitionedKeyNameTaken)
			assert.Equal(t, CodePartitionedKeyNameTaken, OutcomeCode(err))
			assert.Equal(t, before, f.indexes(t, "events_2026_01"), "the occupant is left intact")
			assert.Empty(t, f.keyConstraints(t, "events_2026_01"))
		})

		// A table under the derived name is not an index at all.
		t.Run("table under the derived name", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			leaf := f.leaf(t, "events", "events_2026_01")
			name := derivedKeyName(eventsPrimaryKey, leaf)
			f.exec(t, `CREATE TABLE %[1]s.`+name+` (note text)`)

			_, err := ensureLeafKey(t.Context(), f.pool, eventsPrimaryKey, leaf, leafKeyBudget)

			require.ErrorIs(t, err, ErrPartitionedKeyNameTaken)
			assert.Empty(t, f.indexes(t, "events_2026_01"))
		})

		// A primary key over a nullable column would make ADD CONSTRAINT set
		// NOT NULL, which scans the leaf under ACCESS EXCLUSIVE. The step
		// refuses before it builds anything. A UNIQUE constraint on the same
		// columns has no NOT NULL to set, so it goes ahead.
		t.Run("primary key over a nullable column", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, `
				CREATE TABLE %[1]s.visits (
				    id         bigint,
				    created_at timestamptz NOT NULL
				) PARTITION BY RANGE (created_at)`)
			f.exec(t, `
				CREATE TABLE %[1]s.visits_2026_01 PARTITION OF %[1]s.visits
				    FOR VALUES FROM ('2026-01-01') TO ('2026-02-01')`)
			f.exec(t, `INSERT INTO %[1]s.visits VALUES (1, '2026-01-02'), (2, '2026-01-03')`)
			leaf := f.leaf(t, "visits", "visits_2026_01")
			spec := partitionedKeySpec{kind: partitionedPrimaryKey, name: "visits_pkey", columns: []string{"id", "created_at"}}

			_, err := ensureLeafKey(t.Context(), f.pool, spec, leaf, leafKeyBudget)

			require.ErrorIs(t, err, ErrPartitionedKeyNullableColumn)
			assert.Equal(t, CodePartitionedKeyNullableColumn, OutcomeCode(err))
			assert.Empty(t, f.indexes(t, "visits_2026_01"), "nothing is built")

			unique := partitionedKeySpec{kind: partitionedUniqueKey, name: "visits_id_key", columns: []string{"id", "created_at"}}
			res, err := ensureLeafKey(t.Context(), f.pool, unique, leaf, leafKeyBudget)

			require.NoError(t, err)
			assert.Equal(t, leafKeyBuilt, res.action)
		})

		// A UNIQUE constraint on the key's columns is not the requested
		// primary key, and a parent's primary key could adopt either. The
		// step adds no second constraint.
		t.Run("constraint of the other kind on the same columns", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			leaf := f.leaf(t, "events", "events_2026_01")
			f.exec(t, `ALTER TABLE %[1]s.events_2026_01 ADD CONSTRAINT events_jan_uq UNIQUE (id, created_at)`)

			_, err := ensureLeafKey(t.Context(), f.pool, eventsPrimaryKey, leaf, leafKeyBudget)

			require.ErrorIs(t, err, ErrPartitionedKeyConflict)
			assert.Equal(t, CodePartitionedKeyConflict, OutcomeCode(err))
			assert.Equal(t, []string{"events_jan_uq"}, keys(f.indexes(t, "events_2026_01")))
		})

		// Two UNIQUE constraints that each match the request leave the step
		// no way to choose which one the parent adopts.
		t.Run("two matching constraints", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			leaf := f.leaf(t, "events", "events_2026_01")
			f.exec(t, `ALTER TABLE %[1]s.events_2026_01 ADD CONSTRAINT events_jan_uq1 UNIQUE (id, created_at)`)
			f.exec(t, `ALTER TABLE %[1]s.events_2026_01 ADD CONSTRAINT events_jan_uq2 UNIQUE (id, created_at)`)

			_, err := ensureLeafKey(t.Context(), f.pool, eventsUniqueKey, leaf, leafKeyBudget)

			require.ErrorIs(t, err, ErrPartitionedKeyConflict)
			assert.Len(t, f.keyConstraints(t, "events_2026_01"), 2)
		})

		// A leaf detached after it was listed is no longer the parent's
		// partition. With an index still to build, the step refuses before
		// it builds.
		t.Run("leaf detached before the build", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			leaf := f.leaf(t, "events", "events_2026_01")
			f.exec(t, `ALTER TABLE %[1]s.events DETACH PARTITION %[1]s.events_2026_01`)

			_, err := ensureLeafKey(t.Context(), f.pool, eventsPrimaryKey, leaf, leafKeyBudget)

			require.ErrorIs(t, err, ErrInvariantViolation)
			assert.Contains(t, err.Error(), "ST-7")
			assert.Empty(t, f.indexes(t, "events_2026_01"), "nothing is built on the detached table")
		})

		// A leaf detached after the step read it and built its index, and
		// before the constraint step, is refused under the constraint step's
		// own lock. The index is left for an operator: nothing proves it was
		// this flow's. The constraint step is called directly because the
		// detach has to land between the two steps.
		t.Run("leaf detached before the constraint", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			leaf := f.leaf(t, "events", "events_2026_01")
			name := derivedKeyName(eventsPrimaryKey, leaf)
			f.exec(t, `CREATE UNIQUE INDEX `+name+` ON %[1]s.events_2026_01 (id, created_at)`)
			idx, found, err := relationByName(t.Context(), f.pool, f.schema, name)
			require.NoError(t, err)
			require.True(t, found)
			f.exec(t, `ALTER TABLE %[1]s.events DETACH PARTITION %[1]s.events_2026_01`)

			_, err = addLeafKeyConstraint(t.Context(), briefTx{pool: f.pool}, eventsPrimaryKey, leaf, idx,
				"unique=true USING btree (id, created_at)")

			require.ErrorIs(t, err, ErrInvariantViolation)
			assert.Contains(t, err.Error(), "ST-7")
			assert.Equal(t, []string{name}, keys(f.indexes(t, "events_2026_01")), "the index is left in place")
			assert.Empty(t, f.keyConstraints(t, "events_2026_01"))
		})

		// A leaf dropped after it was listed is refused the same way.
		t.Run("leaf dropped before the step", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			leaf := f.leaf(t, "events", "events_2026_01")
			f.exec(t, `DROP TABLE %[1]s.events_2026_01`)

			_, err := ensureLeafKey(t.Context(), f.pool, eventsPrimaryKey, leaf, leafKeyBudget)

			require.ErrorIs(t, err, ErrInvariantViolation)
			assert.Contains(t, err.Error(), "ST-7")
		})

		// A DEFERRABLE constraint on exactly the key's columns has the same
		// index definition as the requested key, and the parent's immediate
		// constraint would still adopt it, leaving that leaf checked only at
		// commit. The step refuses it as a conflict and adds nothing.
		t.Run("deferrable constraint on the key's columns", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			leaf := f.leaf(t, "events", "events_2026_01")
			f.exec(t, `
				ALTER TABLE %[1]s.events_2026_01
				    ADD CONSTRAINT events_jan_uq UNIQUE (id, created_at)
				    DEFERRABLE INITIALLY DEFERRED`)

			_, err := ensureLeafKey(t.Context(), f.pool, eventsUniqueKey, leaf, leafKeyBudget)

			require.ErrorIs(t, err, ErrPartitionedKeyConflict)
			assert.Equal(t, CodePartitionedKeyConflict, OutcomeCode(err))
			assert.Equal(t, []string{"events_jan_uq"}, keys(f.keyConstraints(t, "events_2026_01")))
		})

		// From PostgreSQL 18 a NOT NULL can be added NOT VALID: the column is
		// marked attnotnull while it still holds a NULL, and ADD CONSTRAINT
		// ... PRIMARY KEY USING INDEX then fails. The step reads the
		// constraint's validation and refuses before it builds anything.
		t.Run("primary key over a NOT VALID not-null column", func(t *testing.T) {
			f := newFixture(t)
			if f.serverMajor(t) < 18 {
				t.Skip("NOT VALID not-null constraints exist from PostgreSQL 18")
			}
			f.exec(t, `
				CREATE TABLE %[1]s.visits (
				    id         bigint,
				    created_at timestamptz NOT NULL
				) PARTITION BY RANGE (created_at)`)
			f.exec(t, `
				CREATE TABLE %[1]s.visits_2026_01 PARTITION OF %[1]s.visits
				    FOR VALUES FROM ('2026-01-01') TO ('2026-02-01')`)
			f.exec(t, `INSERT INTO %[1]s.visits VALUES (NULL, '2026-01-02'), (2, '2026-01-03')`)
			f.exec(t, `ALTER TABLE %[1]s.visits_2026_01 ADD CONSTRAINT visits_jan_id_nn NOT NULL id NOT VALID`)
			leaf := f.leaf(t, "visits", "visits_2026_01")
			spec := partitionedKeySpec{kind: partitionedPrimaryKey, name: "visits_pkey", columns: []string{"id", "created_at"}}

			_, err := ensureLeafKey(t.Context(), f.pool, spec, leaf, leafKeyBudget)

			require.ErrorIs(t, err, ErrPartitionedKeyNullableColumn)
			assert.Empty(t, f.indexes(t, "visits_2026_01"), "nothing is built")
		})

		// The constraint step reads the primary key's columns again under the
		// leaf's lock: a column that lost its NOT NULL after the decision
		// would make ADD CONSTRAINT set NOT NULL and scan the leaf under
		// ACCESS EXCLUSIVE. The constraint step is called directly because
		// the change has to land between the decision and the statement.
		t.Run("not null dropped before the constraint", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, `
				CREATE TABLE %[1]s.visits (
				    id         bigint,
				    created_at timestamptz NOT NULL
				) PARTITION BY RANGE (created_at)`)
			f.exec(t, `
				CREATE TABLE %[1]s.visits_2026_01 PARTITION OF %[1]s.visits
				    FOR VALUES FROM ('2026-01-01') TO ('2026-02-01')`)
			f.exec(t, `ALTER TABLE %[1]s.visits_2026_01 ALTER COLUMN id SET NOT NULL`)
			f.exec(t, `INSERT INTO %[1]s.visits VALUES (1, '2026-01-02'), (2, '2026-01-03')`)
			leaf := f.leaf(t, "visits", "visits_2026_01")
			spec := partitionedKeySpec{kind: partitionedPrimaryKey, name: "visits_pkey", columns: []string{"id", "created_at"}}
			name := derivedKeyName(spec, leaf)
			f.exec(t, `CREATE UNIQUE INDEX `+name+` ON %[1]s.visits_2026_01 (id, created_at)`)
			idx, found, err := relationByName(t.Context(), f.pool, f.schema, name)
			require.NoError(t, err)
			require.True(t, found)
			f.exec(t, `ALTER TABLE %[1]s.visits_2026_01 ALTER COLUMN id DROP NOT NULL`)

			_, err = addLeafKeyConstraint(t.Context(), briefTx{pool: f.pool}, spec, leaf, idx,
				"unique=true USING btree (id, created_at)")

			require.ErrorIs(t, err, ErrPartitionedKeyNullableColumn)
			assert.Equal(t, []string{name}, keys(f.indexes(t, "visits_2026_01")), "the index is left in place")
			assert.Empty(t, f.keyConstraints(t, "visits_2026_01"))
		})

		// A partition whose DETACH PARTITION CONCURRENTLY was cancelled stays
		// in pg_inherits with its detach pending, and is leaving the tree. A
		// leaf listed that way is refused, typed, before anything is read or
		// built.
		t.Run("leaf listed with its detach pending", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			f.cancelDetach(t, "events", "events_2026_01")
			leaf := f.leaf(t, "events", "events_2026_01")
			require.True(t, leaf.detachPending)

			_, err := ensureLeafKey(t.Context(), f.pool, eventsPrimaryKey, leaf, leafKeyBudget)

			require.ErrorIs(t, err, ErrPartitionDetachPending)
			assert.Equal(t, CodePartitionDetachPending, OutcomeCode(err))
			assert.Empty(t, f.indexes(t, "events_2026_01"), "nothing is built")
		})

		// A detach that begins after the leaf was listed is caught by the
		// step's proof that the leaf is still in the tree, which counts only
		// a partition with no detach pending.
		t.Run("detach begun after the listing", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			leaf := f.leaf(t, "events", "events_2026_01")
			require.False(t, leaf.detachPending)
			f.cancelDetach(t, "events", "events_2026_01")

			_, err := ensureLeafKey(t.Context(), f.pool, eventsPrimaryKey, leaf, leafKeyBudget)

			require.ErrorIs(t, err, ErrInvariantViolation)
			assert.Empty(t, f.indexes(t, "events_2026_01"), "nothing is built")
		})
	})
}

// The constraint step locks the leaf SHARE UPDATE EXCLUSIVE before it proves
// the leaf is in the tree, and holds it to the end of its transaction, so no
// DETACH PARTITION can take the leaf out of the tree between the proof and
// the statement. A reader on the leaf holds the step's ADD CONSTRAINT back;
// while it waits, the step's session holds the granted lock. The reader then
// ends and the step finishes.
func TestEnsureLeafKeyConstraintStepHoldsTheLeafInTheTree(t *testing.T) {
	withLeafKeyServer(t, func(t *testing.T, newFixture func(t *testing.T) leafKeyFixture) {
		f := newFixture(t)
		f.events(t)
		leaf := f.leaf(t, "events", "events_2026_01")
		name := derivedKeyName(eventsPrimaryKey, leaf)
		f.exec(t, `CREATE UNIQUE INDEX `+name+` ON %[1]s.events_2026_01 (id, created_at)`)
		idx, found, err := relationByName(t.Context(), f.pool, f.schema, name)
		require.NoError(t, err)
		require.True(t, found)

		reader, err := f.pool.Begin(t.Context())
		require.NoError(t, err)
		defer func() {
			if err := reader.Rollback(context.WithoutCancel(t.Context())); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
				t.Errorf("roll back the reader: %v", err)
			}
		}()
		var readerPID int32
		require.NoError(t, reader.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&readerPID))
		_, err = reader.Exec(t.Context(), fmt.Sprintf(`SELECT count(*) FROM %s.events_2026_01`, f.schema))
		require.NoError(t, err)

		var (
			wg      sync.WaitGroup
			stepErr error
		)
		wg.Go(func() {
			_, stepErr = addLeafKeyConstraint(t.Context(), briefTx{pool: f.pool}, eventsPrimaryKey, leaf, idx,
				"unique=true USING btree (id, created_at)")
		})

		const lockSeenWithin = 2 * time.Second
		assert.Eventually(t, func() bool {
			var held bool
			err := f.pool.QueryRow(t.Context(), `
				SELECT EXISTS (
				    SELECT 1 FROM pg_locks
				     WHERE relation = $1
				       AND mode = 'ShareUpdateExclusiveLock'
				       AND granted
				       AND pid <> $2)`, leaf.oid, readerPID).Scan(&held)
			return err == nil && held
		}, lockSeenWithin, 20*time.Millisecond, "the step's session holds SHARE UPDATE EXCLUSIVE on the leaf while its ADD CONSTRAINT waits")
		require.NoError(t, reader.Rollback(t.Context()))
		wg.Wait()

		require.NoError(t, stepErr)
		constraint, ok := f.keyConstraints(t, "events_2026_01")[name]
		require.True(t, ok, "the step gives the leaf its constraint once the reader ends")
		assert.Equal(t, idx.oid, constraint.indexOID)
	})
}

// A writer under a one-second lock_timeout keeps committing to the leaf for
// the whole step: the concurrent build does not block it, and the constraint
// step holds ACCESS EXCLUSIVE only for a catalog change.
func TestEnsureLeafKeyWritersKeepCommitting(t *testing.T) {
	withLeafKeyServer(t, func(t *testing.T, newFixture func(t *testing.T) leafKeyFixture) {
		f := newFixture(t)
		f.events(t)
		f.exec(t, `
			INSERT INTO %[1]s.events (id, created_at, payload)
			SELECT g, '2026-01-01'::timestamptz + mod(g, 30) * interval '1 day', repeat('x', 200)
			  FROM generate_series(10001, 300000) g`)
		leaf := f.leaf(t, "events", "events_2026_01")

		writer, err := f.pool.Acquire(t.Context())
		require.NoError(t, err)
		defer writer.Release()
		_, err = writer.Exec(t.Context(), `SET lock_timeout = '1s'`)
		require.NoError(t, err)

		ctx, stop := context.WithCancel(t.Context())
		var (
			wg      sync.WaitGroup
			commits atomic.Int64
			failure atomic.Pointer[error]
		)
		insert := fmt.Sprintf(`INSERT INTO %s.events_2026_01 (id, created_at) VALUES ($1, '2026-01-15')`, f.schema)
		wg.Go(func() {
			for id := int64(1_000_000); ctx.Err() == nil; id++ {
				if _, err := writer.Exec(ctx, insert, id); err != nil {
					if ctx.Err() == nil {
						failure.Store(&err)
					}
					return
				}
				commits.Add(1)
			}
		})

		res, err := ensureLeafKey(t.Context(), f.pool, eventsPrimaryKey, leaf, leafKeyBudget)
		during := commits.Load()
		stop()
		wg.Wait()

		require.NoError(t, err)
		assert.Equal(t, leafKeyBuilt, res.action)
		if p := failure.Load(); p != nil {
			require.NoError(t, *p, "the writer must not fail while the step runs")
		}
		assert.Positive(t, during, "the writer commits while the step runs")
	})
}

// After the step has run on every leaf, a plain ADD CONSTRAINT on the parent
// adopts each leaf's constraint instead of building: every leaf constraint
// gets the parent's constraint as its parent, and every leaf index keeps its
// OID and becomes a partition of the parent's index. This is the premise the
// whole flow rests on, pinned on the server the suite runs.
func TestEnsureLeafKeyOutputIsAdoptedByTheParentConstraint(t *testing.T) {
	withLeafKeyServer(t, func(t *testing.T, newFixture func(t *testing.T) leafKeyFixture) {
		f := newFixture(t)
		f.events(t)
		built := map[string]leafKeyResult{}
		for _, name := range []string{"events_2026_01", "events_2026_02"} {
			res, err := ensureLeafKey(t.Context(), f.pool, eventsPrimaryKey, f.leaf(t, "events", name), leafKeyBudget)
			require.NoError(t, err)
			built[name] = res
		}

		f.exec(t, `ALTER TABLE %[1]s.events ADD CONSTRAINT events_pkey PRIMARY KEY (id, created_at)`)

		parent := f.keyConstraints(t, "events")["events_pkey"]
		require.NotZero(t, parent.oid)
		assert.True(t, parent.indexValid, "the parent's index is valid at once")
		for name, res := range built {
			got := f.keyConstraints(t, name)
			require.Len(t, got, 1, "partition %s has only its own constraint", name)
			leaf := got[res.constraint]
			assert.Equal(t, parent.oid, leaf.parentOID, "partition %s's constraint is adopted", name)
			assert.Equal(t, res.indexOID, leaf.indexOID, "partition %s's index keeps its OID", name)
			var attachedTo uint32
			require.NoError(t, f.pool.QueryRow(t.Context(),
				`SELECT inhparent FROM pg_inherits WHERE inhrelid = $1`, res.indexOID).Scan(&attachedTo))
			assert.Equal(t, parent.indexOID, attachedTo, "partition %s's index is a partition of the parent's index", name)
		}
	})
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
