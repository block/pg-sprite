package preflight_test

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/block/pg-sprite/pkg/preflight"
)

// The derived name has the engine's shape, and the same table always
// derives the same name, so a resumed run finds the slot it created.
func TestCopySwapDecodingNameIsStableAndEngineShaped(t *testing.T) {
	name := preflight.CopySwapDecodingName("shop", "app", "ledger")
	assert.Regexp(t, regexp.MustCompile(`^pgsprite_[0-9a-f]{8}$`), name)
	assert.Equal(t, name, preflight.CopySwapDecodingName("shop", "app", "ledger"))
}

// A dot is legal inside a quoted identifier, so two tables of one database
// whose dotted spellings coincide must still derive different names: the
// slot of one is never mistaken for the other's.
func TestCopySwapDecodingNameIsDistinctPerTable(t *testing.T) {
	assert.NotEqual(t,
		preflight.CopySwapDecodingName("shop", "app.v2", "ledger"),
		preflight.CopySwapDecodingName("shop", "app", "v2.ledger"))
	assert.NotEqual(t,
		preflight.CopySwapDecodingName("shop.app", "v2", "ledger"),
		preflight.CopySwapDecodingName("shop", "app.v2", "ledger"))
}

// A slot is cluster-wide while a table is per-database, so the same table
// in two databases derives two names.
func TestCopySwapDecodingNameIsDistinctPerDatabase(t *testing.T) {
	assert.NotEqual(t,
		preflight.CopySwapDecodingName("shop", "app", "ledger"),
		preflight.CopySwapDecodingName("store", "app", "ledger"))
}
