package schemachange

import (
	"errors"
	"fmt"
	"hash/fnv"
)

// ErrNameTooLong reports a derived identifier the server would silently
// truncate to fit NAMEDATALEN; the route refuses instead, because a
// truncated name would no longer be the deterministic one a resume derives.
var ErrNameTooLong = errors.New("derived identifier exceeds PostgreSQL's 63-byte limit")

// maxIdentifierBytes is PostgreSQL's NAMEDATALEN - 1: identifiers longer
// than this are truncated by the server without an error.
const maxIdentifierBytes = 63

// namePrefix marks every relation the copy-and-swap route creates or
// retains, so an operator can list the engine's leftovers by prefix.
const namePrefix = "_pgsprite_"

// NameHash is the sixteen-hex-digit FNV-1a hash of "schema.table" that keys
// every derived copy-and-swap name. The same source table always derives the
// same names, so a resumed run finds the shadow it built earlier.
func NameHash(schema, table string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(schema + "." + table))
	return fmt.Sprintf("%016x", h.Sum64())
}

// ShadowName is the shadow table's name for a source table.
func ShadowName(schema, table string) string {
	return namePrefix + NameHash(schema, table) + "_new"
}

// OldName is the retained source table's name after cutover.
func OldName(schema, table string) string {
	return namePrefix + NameHash(schema, table) + "_old"
}

// OldDependentName is the name an index, extended-statistics object, or
// identity sequence of the retained source table wears after cutover, which
// frees its original name for the shadow's corresponding dependent.
func OldDependentName(schema, table, dependent string) string {
	return OldName(schema, table) + "_" + dependent
}

// CheckIdentifierLengths refuses any derived identifier the server would
// truncate. PostgreSQL measures identifiers in bytes, so a multibyte name is
// checked by its encoded length, not its rune count.
func CheckIdentifierLengths(names ...string) error {
	for _, name := range names {
		if len(name) > maxIdentifierBytes {
			return fmt.Errorf("%w: %q is %d bytes", ErrNameTooLong, name, len(name))
		}
	}
	return nil
}
