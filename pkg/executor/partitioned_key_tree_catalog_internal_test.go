package executor

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A read that covered nothing of a relation has no key columns and no
// missing one; it fails closed rather than return an empty key.
func TestKeyColumnsReadFailsClosedWhenNothingWasRead(t *testing.T) {
	_, err := keyColumnsRead{}.of(partitionNode{oid: 1, schema: "app", name: "events"})

	require.ErrorIs(t, err, ErrInvariantViolation)
}

// The batched key-column read records the first requested column the table
// does not have, by name, and the table is refused rather than read with a
// key one column shorter than the request.
func TestKeyColumnsReadRefusesAMissingKeyColumn(t *testing.T) {
	withPartitionedKeyServer(t, func(t *testing.T, newFixture func(t *testing.T) keyFixture) {
		// events has id but no column nope.
		t.Run("a column the table does not have", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)

			read, columns, err := f.readParentKeyColumns(t, []string{"id", "nope"})

			assert.Equal(t, keyColumnsRead{columns: []leafKeyColumn{eventsID}, lacksColumn: true, missing: "nope"}, read)
			require.Error(t, err)
			assert.NotErrorIs(t, err, ErrInvariantViolation, "a missing column is the request's, not a broken invariant")
			assert.Nil(t, columns)
		})

		// No column has the empty name, so it is missing like any other.
		t.Run("an empty column name", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)

			read, columns, err := f.readParentKeyColumns(t, []string{"id", ""})

			assert.Equal(t, keyColumnsRead{columns: []leafKeyColumn{eventsID}, lacksColumn: true, missing: ""}, read)
			require.Error(t, err)
			assert.Nil(t, columns)
		})

		// A column name is at most 63 bytes, so a 64-byte name matches no
		// column; the server is never asked to take it as a name, which it
		// would refuse with SQLSTATE 42622 rather than read it as missing.
		t.Run("a name too long to be a column name", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			long := strings.Repeat("c", 64)

			read, columns, err := f.readParentKeyColumns(t, []string{"id", long})

			assert.Equal(t, keyColumnsRead{columns: []leafKeyColumn{eventsID}, lacksColumn: true, missing: long}, read)
			require.Error(t, err)
			assert.NotErrorIs(t, err, ErrInvariantViolation, "a missing column is the request's, not a broken invariant")
			assert.Nil(t, columns)
		})

		// The first missing column in key order is the one refused: nope
		// comes before the over-long name, so nope is named.
		t.Run("a column the table does not have before a name too long", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)

			read, columns, err := f.readParentKeyColumns(t, []string{"id", "nope", strings.Repeat("c", 64)})

			assert.Equal(t, keyColumnsRead{columns: []leafKeyColumn{eventsID}, lacksColumn: true, missing: "nope"}, read)
			require.Error(t, err)
			assert.Nil(t, columns)
		})
	})
}

// eventsID is the events fixture's id column as the key-column read finds
// it: the first column, NOT NULL.
var eventsID = leafKeyColumn{name: "id", attnum: 1, notNull: true, quoted: "id"}

// readParentKeyColumns reads the named key columns of the events table with
// the batched read, and returns the read and what of makes of it.
func (f keyFixture) readParentKeyColumns(t *testing.T, names []string) (keyColumnsRead, []leafKeyColumn, error) {
	t.Helper()
	parent := parentNodeOf(indexTarget{tableOID: f.oid(t, "events"), schema: f.schema, table: "events"})
	reads, err := readKeyColumnsOf(t.Context(), f.begin(t), []partitionNode{parent}, names)
	require.NoError(t, err)
	columns, err := reads[parent.oid].of(parent)
	return reads[parent.oid], columns, err
}
