package dbconn_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// The key is the only thing two instances agree on. They never talk to each
// other: each hashes the table's name and asks the server for that number, so
// exclusion holds exactly when both derive the same value. Two builds of
// pg-sprite are two instances, which makes the derivation a compatibility
// contract with every version already deployed — changing it would not break
// a test in an obvious way, it would quietly stop a new instance from
// excluding an old one. These values pin it.
func TestTableLockKeyIsPinned(t *testing.T) {
	for _, tt := range []struct {
		database, schema, table string
		want                    int64
	}{
		{"app", "public", "orders", 6365524024423038674},
		{"app", "public", "customers", 8118386634440229320},
		{"app", "billing", "orders", -2749345589207798260},
		{"other", "public", "orders", 2343726829005854157},
		{"", "", "", -2788552698171019337},
	} {
		t.Run(tt.database+"."+tt.schema+"."+tt.table, func(t *testing.T) {
			assert.Equal(t, tt.want, dbconn.TableLockKey(tt.database, tt.schema, tt.table))
		})
	}
}

// Each part is hashed with a terminator, so a character moving across a
// boundary changes the key. Without one, a table named "x" in schema "ab"
// and a table named "bx" in schema "a" would hash to the same value and one
// would exclude the other for no reason.
func TestTableLockKeySeparatesItsParts(t *testing.T) {
	require.NotEqual(t,
		dbconn.TableLockKey("app", "ab", "x"),
		dbconn.TableLockKey("app", "a", "bx"))
}
