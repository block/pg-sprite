package schemadiff

import "regexp"

// engineRelationPattern is the shape of every relation name the
// copy-and-swap route derives in a target's schema: the "_pgsprite_" prefix,
// the sixteen-lowercase-hex-digit hash of the source table's qualified name,
// then the shadow's "_new", the retained source's "_old", or a retained
// source dependent's "_old_" and the dependent's own sixteen-digit hash. The
// derivers are pkg/schemachange's ShadowName, OldName, and OldDependentName;
// they import this package, so the shape lives here where the managed-table
// listing can apply it, and a test beside the derivers holds every one of
// them to it.
var engineRelationPattern = regexp.MustCompile(`^_pgsprite_[0-9a-f]{16}_(new|old|old_[0-9a-f]{16})$`)

// IsEngineRelationName reports whether name has the shape of a relation the
// copy-and-swap route names in a target's schema: a shadow table, a source
// table retained after its swap, or an index, extended-statistics object, or
// identity sequence of that retained table. Such a relation is the engine's
// own, not a table a desired file declares, so ListManagedTables leaves it
// out; a host that enumerates a schema's relations its own way applies the
// same test. The check reads the name alone and needs no database: the
// "_pgsprite_" prefix is the engine's, so a user relation that copies the
// exact shape is treated as the engine's too. A name that only wears the
// prefix, such as _pgsprite_orders, is not the engine's.
//
// The shadow's own indexes, constraints, and sequences carry names the
// server chose from the shadow's name, not names the engine derives, so they
// do not match; they belong to the shadow table and leave with it.
func IsEngineRelationName(name string) bool {
	return engineRelationPattern.MatchString(name)
}
