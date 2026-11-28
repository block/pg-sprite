package executor

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests run the index step against the server: it gives a table every
// index and key constraint of the partitioned parent before the attach, and
// the ATTACH that follows takes those indexes and builds none of its own.

var attachIndexBudget = ConcurrentBudget{Overall: time.Minute}

// standalone resolves a table that is not a partition yet.
func (f attachProofFixture) standalone(t *testing.T, table string) partitionNode {
	t.Helper()
	var oid uint32
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT $1::regclass::oid",
		pgx.Identifier{f.schema, table}.Sanitize()).Scan(&oid))
	return standaloneTable(oid, f.schema, table)
}

// parentOID resolves the partitioned parent.
func (f attachProofFixture) parentOID(t *testing.T, table string) uint32 {
	t.Helper()
	return f.standalone(t, table).oid
}

// tableIndexes maps each index on the table to its OID and whether it is
// attached to a partitioned index.
type attachedIndex struct {
	oid      uint32
	attached bool
}

func (f attachProofFixture) tableIndexes(t *testing.T, table string) map[string]attachedIndex {
	t.Helper()
	rows, err := f.pool.Query(t.Context(), `SELECT c.relname, c.oid, c.relispartition
		FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class c ON c.oid = i.indexrelid
		WHERE i.indrelid = $1::regclass`, pgx.Identifier{f.schema, table}.Sanitize())
	require.NoError(t, err)
	defer rows.Close()
	got := map[string]attachedIndex{}
	for rows.Next() {
		var name string
		var idx attachedIndex
		require.NoError(t, rows.Scan(&name, &idx.oid, &idx.attached))
		got[name] = idx
	}
	require.NoError(t, rows.Err())
	return got
}

// ordersParent is a partitioned table with every index kind the step gives
// a table: a plain index, an expression index, a plain unique index, a
// primary key, and a UNIQUE constraint. orders_2027 is the table to attach,
// with no index at all and the primary key's columns NOT NULL.
const ordersParent = `
	CREATE TABLE %[1]s.orders (
		id bigint NOT NULL,
		placed date NOT NULL,
		ref text,
		email text,
		CONSTRAINT orders_pkey PRIMARY KEY (id, placed),
		CONSTRAINT orders_ref_key UNIQUE (ref, placed)
	) PARTITION BY RANGE (placed);
	CREATE INDEX orders_ref_idx ON %[1]s.orders (ref);
	CREATE INDEX orders_email_lower_idx ON %[1]s.orders (lower(email));
	CREATE UNIQUE INDEX orders_email_placed_idx ON %[1]s.orders (email, placed);
	CREATE TABLE %[1]s.orders_2026 PARTITION OF %[1]s.orders FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
	CREATE TABLE %[1]s.orders_2027 (
		id bigint NOT NULL,
		placed date NOT NULL,
		ref text,
		email text
	);
	INSERT INTO %[1]s.orders_2027 VALUES (1, '2027-03-01', 'r1', 'A@x'), (2, '2027-04-01', 'r2', 'b@x')`

const attachOrders2027 = `ALTER TABLE %[1]s.orders
	ATTACH PARTITION %[1]s.orders_2027
	FOR VALUES FROM ('2027-01-01') TO ('2028-01-01')`

// From a table with no index, the step builds the three plain indexes and
// gives the table both key constraints. A rerun finds all five and builds
// nothing. The ATTACH then attaches exactly those five indexes, by OID, and
// creates none.
func TestEnsureAttachIndexesGivesTheTableEveryIndexAndTheAttachBuildsNone(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, ordersParent)
		parent, table := f.parentOID(t, "orders"), f.standalone(t, "orders_2027")

		results, err := ensureAttachIndexes(t.Context(), f.pool, parent, table, attachIndexBudget)
		require.NoError(t, err)
		actions := map[string]attachIndexAction{}
		for _, r := range results {
			actions[r.parentIndex] = r.action
		}
		assert.Equal(t, map[string]attachIndexAction{
			"orders_pkey":             attachIndexKeyed,
			"orders_ref_key":          attachIndexKeyed,
			"orders_ref_idx":          attachIndexBuilt,
			"orders_email_lower_idx":  attachIndexBuilt,
			"orders_email_placed_idx": attachIndexBuilt,
		}, actions)
		before := f.tableIndexes(t, "orders_2027")
		require.Len(t, before, 5)

		again, err := ensureAttachIndexes(t.Context(), f.pool, parent, table, attachIndexBudget)
		require.NoError(t, err)
		for _, r := range again {
			assert.Equal(t, attachIndexFound, r.action, "a rerun finds %s's index", r.parentIndex)
		}
		assert.Equal(t, before, f.tableIndexes(t, "orders_2027"), "a rerun builds nothing")

		f.attach(t, attachOrders2027)
		after := f.tableIndexes(t, "orders_2027")
		require.Len(t, after, 5, "the attach created no index of its own")
		for name, idx := range after {
			assert.Equal(t, before[name].oid, idx.oid, "%s is the index the step gave the table", name)
			assert.True(t, idx.attached, "%s is attached to the parent's index", name)
		}
	})
}

// A matching index the table already has, under any name, is taken rather
// than built again. Two parent indexes of the same definition need two
// indexes on the table, because the attach takes each for one parent index
// only, so the second is built.
func TestEnsureAttachIndexesTakesAMatchingIndexOnceEach(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, `
			CREATE TABLE %[1]s.events (
				id bigint NOT NULL,
				day date NOT NULL
			) PARTITION BY RANGE (day);
			CREATE INDEX events_id_a ON %[1]s.events (id);
			CREATE INDEX events_id_b ON %[1]s.events (id);
			CREATE TABLE %[1]s.events_2027 (
				id bigint NOT NULL,
				day date NOT NULL
			);
			CREATE INDEX events_2027_own_id ON %[1]s.events_2027 (id)`)
		parent, table := f.parentOID(t, "events"), f.standalone(t, "events_2027")
		own := f.tableIndexes(t, "events_2027")["events_2027_own_id"].oid

		results, err := ensureAttachIndexes(t.Context(), f.pool, parent, table, attachIndexBudget)
		require.NoError(t, err)
		require.Len(t, results, 2)
		assert.Equal(t, attachIndexFound, results[0].action)
		assert.Equal(t, own, results[0].indexOID, "the table's own index is taken for the first")
		assert.Equal(t, attachIndexBuilt, results[1].action, "the second needs an index of its own")
		assert.NotEqual(t, own, results[1].indexOID)

		f.attach(t, `ALTER TABLE %[1]s.events ATTACH PARTITION %[1]s.events_2027
			FOR VALUES FROM ('2027-01-01') TO ('2028-01-01')`)
		assert.Len(t, f.tableIndexes(t, "events_2027"), 2, "the attach created no index of its own")
	})
}

// An invalid index under the derived name is an earlier run's failed build:
// it is removed and the index built again, under the same name, valid.
func TestEnsureAttachIndexesRebuildsAnEarlierRunsInvalidIndex(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, `
			CREATE TABLE %[1]s.events (
				id bigint NOT NULL,
				day date NOT NULL
			) PARTITION BY RANGE (day);
			CREATE INDEX events_id_idx ON %[1]s.events (id);
			CREATE TABLE %[1]s.events_2027 (
				id bigint NOT NULL,
				day date NOT NULL
			)`)
		parent, table := f.parentOID(t, "events"), f.standalone(t, "events_2027")
		derived := derivedPartitionIndexName("events_id_idx", table.oid)
		f.exec(t, `CREATE INDEX `+pgx.Identifier{derived}.Sanitize()+` ON %[1]s.events_2027 (id)`)
		debris := f.tableIndexes(t, "events_2027")[derived].oid
		_, err := f.pool.Exec(t.Context(), `UPDATE pg_catalog.pg_index SET indisvalid = false WHERE indexrelid = $1`, debris)
		require.NoError(t, err)

		results, err := ensureAttachIndexes(t.Context(), f.pool, parent, table, attachIndexBudget)
		require.NoError(t, err)
		require.Len(t, results, 1)
		assert.Equal(t, attachIndexBuilt, results[0].action)
		assert.Equal(t, derived, results[0].index)
		assert.NotEqual(t, debris, results[0].indexOID, "the debris was dropped and the index built anew")
		var valid bool
		require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT indisvalid FROM pg_catalog.pg_index WHERE indexrelid = $1`,
			results[0].indexOID).Scan(&valid))
		assert.True(t, valid)
	})
}

// Indexes the step cannot give the table are refused, typed, before
// anything is built for them: an EXCLUDE constraint's, and a UNIQUE
// constraint whose index carries an INCLUDE column, which the leaf key step
// does not build.
func TestEnsureAttachIndexesRefusesAnIndexItCannotGive(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		// A partitioned table takes an EXCLUDE constraint from PostgreSQL 17.
		t.Run("an EXCLUDE constraint", func(t *testing.T) {
			f := newFixture(t)
			var major int
			require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT current_setting('server_version_num')::int / 10000").Scan(&major))
			if major < 17 {
				t.Skipf("PostgreSQL %d refuses an EXCLUDE constraint on a partitioned table", major)
			}
			f.exec(t, `
				CREATE TABLE %[1]s.rooms (
					room integer NOT NULL,
					day date NOT NULL,
					CONSTRAINT rooms_no_double EXCLUDE USING btree (room WITH =, day WITH =)
				) PARTITION BY RANGE (day);
				CREATE TABLE %[1]s.rooms_2027 (
					room integer NOT NULL,
					day date NOT NULL
				)`)
			_, err := ensureAttachIndexes(t.Context(), f.pool, f.parentOID(t, "rooms"), f.standalone(t, "rooms_2027"), attachIndexBudget)
			require.ErrorIs(t, err, errAttachIndexUnsupported)
			assert.Empty(t, f.tableIndexes(t, "rooms_2027"))
		})

		t.Run("a UNIQUE constraint with an INCLUDE column", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, `
				CREATE TABLE %[1]s.events (
					id bigint NOT NULL,
					day date NOT NULL,
					note text,
					CONSTRAINT events_id_key UNIQUE (id, day) INCLUDE (note)
				) PARTITION BY RANGE (day);
				CREATE TABLE %[1]s.events_2027 (
					id bigint NOT NULL,
					day date NOT NULL,
					note text
				)`)
			_, err := ensureAttachIndexes(t.Context(), f.pool, f.parentOID(t, "events"), f.standalone(t, "events_2027"), attachIndexBudget)
			require.ErrorIs(t, err, errAttachIndexUnsupported)
			assert.Empty(t, f.tableIndexes(t, "events_2027"))
		})
	})
}

// The derived name held by another relation is not taken from it, and a
// table that became a partition since it was resolved is no longer the
// standalone table the step builds on.
func TestEnsureAttachIndexesRefusesATakenNameAndAMovedTable(t *testing.T) {
	const events = `
		CREATE TABLE %[1]s.events (
			id bigint NOT NULL,
			day date NOT NULL
		) PARTITION BY RANGE (day);
		CREATE INDEX events_id_idx ON %[1]s.events (id);
		CREATE TABLE %[1]s.events_2027 (
			id bigint NOT NULL,
			day date NOT NULL
		)`
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		t.Run("the derived name is a sequence", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, events)
			table := f.standalone(t, "events_2027")
			f.exec(t, `CREATE SEQUENCE %[1]s.`+pgx.Identifier{derivedPartitionIndexName("events_id_idx", table.oid)}.Sanitize())
			_, err := ensureAttachIndexes(t.Context(), f.pool, f.parentOID(t, "events"), table, attachIndexBudget)
			require.ErrorIs(t, err, ErrPartitionedIndexNameTaken)
			assert.Empty(t, f.tableIndexes(t, "events_2027"))
		})

		t.Run("the table was attached by someone else", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, `
				CREATE TABLE %[1]s.events (
					id bigint NOT NULL,
					day date NOT NULL
				) PARTITION BY RANGE (day);
				CREATE TABLE %[1]s.events_2027 (
					id bigint NOT NULL,
					day date NOT NULL
				)`)
			table := f.standalone(t, "events_2027")
			f.exec(t, `
				ALTER TABLE %[1]s.events ATTACH PARTITION %[1]s.events_2027 FOR VALUES FROM ('2027-01-01') TO ('2028-01-01');
				CREATE INDEX events_id_idx ON ONLY %[1]s.events (id)`)
			_, err := ensureAttachIndexes(t.Context(), f.pool, f.parentOID(t, "events"), table, attachIndexBudget)
			require.ErrorIs(t, err, ErrInvariantViolation)
			assert.Empty(t, f.tableIndexes(t, "events_2027"))
		})
	})
}

// attachKeepsTheIndexes asserts the results name a distinct table index for
// each parent index, runs the ATTACH, and asserts it attached exactly the
// table's indexes, by OID, and created none.
func (f attachProofFixture) attachKeepsTheIndexes(t *testing.T, table string, results []attachIndexResult, attachSQL string) {
	t.Helper()
	distinct := map[uint32]string{}
	for _, r := range results {
		other, dup := distinct[r.indexOID]
		require.False(t, dup, "%s and %s both report table index %d", other, r.parentIndex, r.indexOID)
		distinct[r.indexOID] = r.parentIndex
	}
	before := f.tableIndexes(t, table)
	f.attach(t, attachSQL)
	after := f.tableIndexes(t, table)
	require.Len(t, after, len(before), "the attach created no index of its own")
	for name, idx := range after {
		assert.Equal(t, before[name].oid, idx.oid, "%s is the index the table had", name)
		assert.True(t, idx.attached, "%s is attached to the parent's index", name)
	}
	for oid, parentIndex := range distinct {
		var attached bool
		require.NoError(t, f.pool.QueryRow(t.Context(),
			`SELECT relispartition FROM pg_catalog.pg_class WHERE oid = $1`, oid).Scan(&attached))
		assert.True(t, attached, "the index reported for %s is attached", parentIndex)
	}
}

const attachEvents2027 = `ALTER TABLE %[1]s.events ATTACH PARTITION %[1]s.events_2027
	FOR VALUES FROM ('2027-01-01') TO ('2028-01-01')`

// Two parent indexes of one definition each need a table index of their
// own: the attach gives a table index to one parent index only. From a
// table with none, the step builds two.
func TestEnsureAttachIndexesBuildsAnIndexForEachOfTwoIdenticalParentIndexes(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, `
			CREATE TABLE %[1]s.events (
				id bigint NOT NULL,
				day date NOT NULL
			) PARTITION BY RANGE (day);
			CREATE INDEX events_id_a ON %[1]s.events (id);
			CREATE INDEX events_id_b ON %[1]s.events (id);
			CREATE TABLE %[1]s.events_2027 (
				id bigint NOT NULL,
				day date NOT NULL
			)`)
		parent, table := f.parentOID(t, "events"), f.standalone(t, "events_2027")

		results, err := ensureAttachIndexes(t.Context(), f.pool, parent, table, attachIndexBudget)
		require.NoError(t, err)
		require.Len(t, results, 2)
		assert.Equal(t, attachIndexBuilt, results[0].action)
		assert.Equal(t, attachIndexBuilt, results[1].action)
		f.attachKeepsTheIndexes(t, "events_2027", results, attachEvents2027)
	})
}

// The table index under the second parent index's derived name is the one
// the attach gives the first, which comes first in OID order, so it cannot
// count for the second, and the second's index cannot be built under that
// name. The step refuses rather than report one index for both.
func TestEnsureAttachIndexesRefusesTheSecondsNameHeldByTheFirstsIndex(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, `
			CREATE TABLE %[1]s.events (
				id bigint NOT NULL,
				day date NOT NULL
			) PARTITION BY RANGE (day);
			CREATE INDEX events_id_a ON %[1]s.events (id);
			CREATE INDEX events_id_b ON %[1]s.events (id);
			CREATE TABLE %[1]s.events_2027 (
				id bigint NOT NULL,
				day date NOT NULL
			)`)
		parent, table := f.parentOID(t, "events"), f.standalone(t, "events_2027")
		f.exec(t, `CREATE INDEX `+pgx.Identifier{derivedPartitionIndexName("events_id_b", table.oid)}.Sanitize()+
			` ON %[1]s.events_2027 (id)`)
		before := f.tableIndexes(t, "events_2027")

		_, err := ensureAttachIndexes(t.Context(), f.pool, parent, table, attachIndexBudget)
		require.ErrorIs(t, err, ErrPartitionedIndexNameTaken)
		assert.Equal(t, before, f.tableIndexes(t, "events_2027"), "nothing was built")
	})
}

// A parent's plain unique index and its UNIQUE constraint on the same
// columns each need a table index: the constraint one backed by a UNIQUE
// constraint of the table. The key step does not adopt the plain index the
// step built for the first.
func TestEnsureAttachIndexesGivesAPlainUniqueIndexAndAUniqueConstraintAnIndexEach(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, `
			CREATE TABLE %[1]s.events (
				id bigint NOT NULL,
				day date NOT NULL
			) PARTITION BY RANGE (day);
			CREATE UNIQUE INDEX events_id_day_idx ON %[1]s.events (id, day);
			ALTER TABLE %[1]s.events ADD CONSTRAINT events_id_day_key UNIQUE (id, day);
			CREATE TABLE %[1]s.events_2027 (
				id bigint NOT NULL,
				day date NOT NULL
			)`)
		parent, table := f.parentOID(t, "events"), f.standalone(t, "events_2027")

		results, err := ensureAttachIndexes(t.Context(), f.pool, parent, table, attachIndexBudget)
		require.NoError(t, err)
		require.Len(t, results, 2)
		assert.Equal(t, "events_id_day_idx", results[0].parentIndex)
		assert.Equal(t, attachIndexBuilt, results[0].action)
		assert.Equal(t, "events_id_day_key", results[1].parentIndex)
		assert.Equal(t, attachIndexKeyed, results[1].action)
		f.attachKeepsTheIndexes(t, "events_2027", results, attachEvents2027)
	})
}

// The attach gives a plain parent index a table index that backs a
// constraint, when it is the first match, so the step takes the table's
// UNIQUE constraint's index rather than build a second unique index the
// attach would leave unattached.
func TestEnsureAttachIndexesTakesAConstraintsIndexForAPlainParentIndex(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, `
			CREATE TABLE %[1]s.events (
				id bigint NOT NULL,
				day date NOT NULL
			) PARTITION BY RANGE (day);
			CREATE UNIQUE INDEX events_id_day_idx ON %[1]s.events (id, day);
			CREATE TABLE %[1]s.events_2027 (
				id bigint NOT NULL,
				day date NOT NULL,
				CONSTRAINT events_2027_id_day_key UNIQUE (id, day)
			)`)
		parent, table := f.parentOID(t, "events"), f.standalone(t, "events_2027")
		own := f.tableIndexes(t, "events_2027")["events_2027_id_day_key"].oid

		results, err := ensureAttachIndexes(t.Context(), f.pool, parent, table, attachIndexBudget)
		require.NoError(t, err)
		require.Len(t, results, 1)
		assert.Equal(t, attachIndexFound, results[0].action)
		assert.Equal(t, own, results[0].indexOID)
		f.attachKeepsTheIndexes(t, "events_2027", results, attachEvents2027)
	})
}

// A foreign key of the parent to itself names the primary key's index as
// the one it references, and is not a constraint that index backs: the
// step gives the table the primary key and refuses nothing.
func TestEnsureAttachIndexesGivesAKeyASelfReferenceDoesNotRefuse(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, `
			CREATE TABLE %[1]s.nodes (
				id bigint NOT NULL,
				day date NOT NULL,
				parent bigint,
				CONSTRAINT nodes_pkey PRIMARY KEY (id, day)
			) PARTITION BY RANGE (day);
			ALTER TABLE %[1]s.nodes ADD CONSTRAINT nodes_parent_fk
				FOREIGN KEY (parent, day) REFERENCES %[1]s.nodes (id, day);
			CREATE TABLE %[1]s.nodes_2027 (
				id bigint NOT NULL,
				day date NOT NULL,
				parent bigint
			)`)
		parent, table := f.parentOID(t, "nodes"), f.standalone(t, "nodes_2027")

		results, err := ensureAttachIndexes(t.Context(), f.pool, parent, table, attachIndexBudget)
		require.NoError(t, err)
		require.Len(t, results, 1)
		assert.Equal(t, "nodes_pkey", results[0].parentIndex)
		assert.Equal(t, attachIndexKeyed, results[0].action)
		f.attachKeepsTheIndexes(t, "nodes_2027", results, `ALTER TABLE %[1]s.nodes ATTACH PARTITION %[1]s.nodes_2027
			FOR VALUES FROM ('2027-01-01') TO ('2028-01-01')`)
	})
}

// A DEFERRABLE key checks uniqueness only at commit; the leaf key step
// gives the table an immediate one, so the step refuses it before it
// builds anything.
func TestEnsureAttachIndexesRefusesADeferrableKey(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, `
			CREATE TABLE %[1]s.events (
				id bigint NOT NULL,
				day date NOT NULL,
				CONSTRAINT events_id_key UNIQUE (id, day) DEFERRABLE
			) PARTITION BY RANGE (day);
			CREATE TABLE %[1]s.events_2027 (
				id bigint NOT NULL,
				day date NOT NULL
			)`)

		_, err := ensureAttachIndexes(t.Context(), f.pool, f.parentOID(t, "events"), f.standalone(t, "events_2027"), attachIndexBudget)
		require.ErrorIs(t, err, errAttachIndexUnsupported)
		assert.Empty(t, f.tableIndexes(t, "events_2027"))
	})
}

// The step takes a standalone plain table and a partitioned parent and
// nothing else: a partition already in the tree and a parent that is not
// partitioned are caller bugs, refused fail-closed; an inheritance child is
// not a partition of nothing; and a partitioned table to attach is a shape
// the step does not handle.
func TestEnsureAttachIndexesRefusesATableOrParentItDoesNotTake(t *testing.T) {
	const events = `
		CREATE TABLE %[1]s.events (
			id bigint NOT NULL,
			day date NOT NULL
		) PARTITION BY RANGE (day);
		CREATE INDEX events_id_idx ON %[1]s.events (id);`
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		t.Run("a partition, not a standalone table", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, events+`
				CREATE TABLE %[1]s.events_2026 PARTITION OF %[1]s.events
					FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')`)
			parent := f.parentOID(t, "events")
			leaf := f.standalone(t, "events_2026")
			leaf.standalone, leaf.parentOID = false, parent
			before := f.tableIndexes(t, "events_2026")

			_, err := ensureAttachIndexes(t.Context(), f.pool, parent, leaf, attachIndexBudget)
			require.ErrorIs(t, err, ErrInvariantViolation)
			assert.Equal(t, before, f.tableIndexes(t, "events_2026"))
		})

		t.Run("an inheritance child", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, events+`
				CREATE TABLE %[1]s.base (
					id bigint NOT NULL,
					day date NOT NULL
				);
				CREATE TABLE %[1]s.events_2027 () INHERITS (%[1]s.base)`)

			_, err := ensureAttachIndexes(t.Context(), f.pool, f.parentOID(t, "events"), f.standalone(t, "events_2027"), attachIndexBudget)
			require.ErrorIs(t, err, ErrInvariantViolation)
			assert.Empty(t, f.tableIndexes(t, "events_2027"))
		})

		t.Run("a parent that is a plain table", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, `
				CREATE TABLE %[1]s.events (
					id bigint NOT NULL,
					day date NOT NULL
				);
				CREATE TABLE %[1]s.events_2027 (
					id bigint NOT NULL,
					day date NOT NULL
				)`)

			_, err := ensureAttachIndexes(t.Context(), f.pool, f.parentOID(t, "events"), f.standalone(t, "events_2027"), attachIndexBudget)
			require.ErrorIs(t, err, ErrInvariantViolation)
		})

		t.Run("a partitioned table to attach", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, events+`
				CREATE TABLE %[1]s.events_2027 (
					id bigint NOT NULL,
					day date NOT NULL
				) PARTITION BY RANGE (day)`)
			table := f.standalone(t, "events_2027")
			table.relkind = "p"

			_, err := ensureAttachIndexes(t.Context(), f.pool, f.parentOID(t, "events"), table, attachIndexBudget)
			require.ErrorIs(t, err, errAttachIndexUnsupported)
			assert.Empty(t, f.tableIndexes(t, "events_2027"))
		})
	})
}
