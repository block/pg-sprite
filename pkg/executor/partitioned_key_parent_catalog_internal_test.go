package executor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The decision of what the partitioned table app.events (OID 10) already
// carries, made from catalog facts as checkParent reads them, for the key
// (id, created_at).
func TestDecideParentKey(t *testing.T) {
	parent := partitionNode{oid: 10, schema: "app", name: "events", relkind: "p"}
	notNull := []leafKeyColumn{
		{name: "id", attnum: 1, notNull: true, quoted: "id"},
		{name: "created_at", attnum: 2, notNull: true, quoted: "created_at"},
	}
	// parentIndex is the valid partitioned index a parent's key constraint
	// over (id, created_at) carries.
	parentIndex := func(name string) relationFacts {
		return relationFacts{
			oid: 20, schema: "app", name: name, relkind: "I",
			tableOID: 10, tableName: "events", tableRelkind: "p",
			valid: true, unique: true, backsConstraint: true,
			def:        "CREATE UNIQUE INDEX " + name + " ON ONLY app.events USING btree (id, created_at)",
			quotedName: name, quotedSchema: "app", quotedTable: "events",
		}
	}
	requested := parentConstraint{oid: 30, name: "events_pkey", contype: "p", key: []int16{1, 2}, index: parentIndex("events_pkey"), hasIndex: true}

	t.Run("a parent without the constraint may be given it", func(t *testing.T) {
		_, found, err := decideParentKey(eventsPrimaryKey, parent, notNull, nil, relationFacts{}, false)

		require.NoError(t, err)
		assert.False(t, found)
	})

	// A run that stopped after the parent's ADD CONSTRAINT committed finds it
	// and reports it, rather than refuse the partitions' adopted constraints.
	t.Run("exactly the requested constraint is found", func(t *testing.T) {
		got, found, err := decideParentKey(eventsPrimaryKey, parent, notNull, []parentConstraint{requested}, parentIndex("events_pkey"), true)

		require.NoError(t, err)
		assert.True(t, found)
		assert.Equal(t, uint32(30), got.oid)
	})

	for _, tc := range []struct {
		name  string
		named parentConstraint
	}{
		{"a CHECK constraint under the name", parentConstraint{oid: 31, name: "events_pkey", contype: "c"}},
		{"a unique constraint under the name", func() parentConstraint {
			c := requested
			c.contype = "u"
			return c
		}()},
		{"the key on other columns under the name", func() parentConstraint {
			c := requested
			c.key = []int16{1}
			c.index.def = "CREATE UNIQUE INDEX events_pkey ON ONLY app.events USING btree (id)"
			return c
		}()},
		{"a deferrable key under the name", func() parentConstraint {
			c := requested
			c.deferrable = true
			return c
		}()},
		{"the key with an invalid index under the name", func() parentConstraint {
			c := requested
			c.index.valid = false
			return c
		}()},
		{"the key with INCLUDE under the name", func() parentConstraint {
			c := requested
			c.index.def = "CREATE UNIQUE INDEX events_pkey ON ONLY app.events USING btree (id, created_at) INCLUDE (payload)"
			return c
		}()},
	} {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			_, _, err := decideParentKey(eventsPrimaryKey, parent, notNull, []parentConstraint{tc.named}, relationFacts{}, false)

			require.ErrorIs(t, err, ErrPartitionedKeyParentConflict)
		})
	}

	t.Run("refuses a primary key when the parent has another", func(t *testing.T) {
		other := parentConstraint{oid: 32, name: "events_old_pkey", contype: "p", key: []int16{1, 2}, index: parentIndex("events_old_pkey"), hasIndex: true}

		_, _, err := decideParentKey(eventsPrimaryKey, parent, notNull, []parentConstraint{other}, relationFacts{}, false)

		require.ErrorIs(t, err, ErrPartitionedKeyParentConflict)
	})

	t.Run("a unique constraint beside a primary key may be added", func(t *testing.T) {
		pk := parentConstraint{oid: 32, name: "events_old_pkey", contype: "p", key: []int16{1}, index: parentIndex("events_old_pkey"), hasIndex: true}

		_, found, err := decideParentKey(eventsUniqueKey, parent, notNull, []parentConstraint{pk}, relationFacts{}, false)

		require.NoError(t, err)
		assert.False(t, found)
	})

	// The parent's index takes the constraint's name, so the ADD CONSTRAINT
	// would fail only at the very end, after every partition was built.
	t.Run("refuses a name another relation holds", func(t *testing.T) {
		holder := relationFacts{oid: 40, schema: "app", name: "events_pkey", relkind: "i", tableName: "other"}

		_, _, err := decideParentKey(eventsPrimaryKey, parent, notNull, nil, holder, true)

		require.ErrorIs(t, err, ErrPartitionedKeyParentConflict)
	})

	t.Run("refuses a primary key over a column the parent allows to be NULL", func(t *testing.T) {
		nullable := []leafKeyColumn{notNull[0], {name: "created_at", attnum: 2, quoted: "created_at"}}

		_, _, err := decideParentKey(eventsPrimaryKey, parent, nullable, nil, relationFacts{}, false)

		require.ErrorIs(t, err, ErrPartitionedKeyNullableColumn)
	})

	t.Run("a unique constraint over a column the parent allows to be NULL may be added", func(t *testing.T) {
		nullable := []leafKeyColumn{notNull[0], {name: "created_at", attnum: 2, quoted: "created_at"}}

		_, found, err := decideParentKey(eventsUniqueKey, parent, nullable, nil, relationFacts{}, false)

		require.NoError(t, err)
		assert.False(t, found)
	})
}
