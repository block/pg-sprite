package schemadiff

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The three shapes the copy-and-swap route derives: the shadow, the source
// retained after its swap, and a dependent of the retained source. The hash
// is the one schemachange derives for public.orders; the deriver side of
// the contract is held in pkg/schemachange's names test.
func TestIsEngineRelationNameMatchesEachDerivedShape(t *testing.T) {
	assert.True(t, IsEngineRelationName("_pgsprite_c74da4a21f58f903_new"), "the shadow table")
	assert.True(t, IsEngineRelationName("_pgsprite_c74da4a21f58f903_old"), "the retained source")
	assert.True(t, IsEngineRelationName("_pgsprite_c74da4a21f58f903_old_9b1c34e0a7f2d685"), "a retained source's index or identity sequence")
}

// A name that only wears the prefix is a user's: the shape needs the exact
// hash width, lowercase hex, and one of the derived suffixes, so none of
// these is hidden from the managed-table listing.
func TestIsEngineRelationNameRejectsLookAlikes(t *testing.T) {
	for name, why := range map[string]string{
		"orders":                                          "an ordinary table",
		"_pgsprite_orders":                                "the prefix on a user name",
		"_pgsprite_foo_old":                               "a user table with the old suffix and no hash",
		"_pgsprite_c74da4a21f58f90_old":                   "a fifteen-digit hash",
		"_pgsprite_c74da4a21f58f9030_old":                 "a seventeen-digit hash",
		"_pgsprite_C74DA4A21F58F903_old":                  "uppercase hex, which the deriver never writes",
		"_pgsprite_c74da4a21f58f903":                      "a hash with no suffix",
		"_pgsprite_c74da4a21f58f903_older":                "a suffix that only starts with old",
		"_pgsprite_c74da4a21f58f903_new_pkey":             "the server's name for a shadow's index",
		"_pgsprite_c74da4a21f58f903_new_9b1c34e0a7f2d685": "a dependent hash on the shadow, which is never derived",
		"_pgsprite_c74da4a21f58f903_old_9b1c34e0":         "a short dependent hash",
		"pgsprite_0d00ea85":                               "a replication slot name, not a relation",
		"x_pgsprite_c74da4a21f58f903_old":                 "a name that only contains the shape",
		"_pgsprite_c74da4a21f58f903_old\n":                "a trailing newline",
	} {
		assert.False(t, IsEngineRelationName(name), "%q: %s", name, why)
	}
}
