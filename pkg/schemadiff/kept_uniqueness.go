package schemadiff

import (
	"slices"
	"strings"

	"github.com/block/pg-sprite/pkg/statement"
)

// uniquenessKeeper returns the name of the desired unique index or key that
// rejects every duplicate the unique index or unique constraint ch drops
// rejects, and is in place when ch runs, or "" when there is none or ch
// drops something else. An object is in place when the live table already
// has it as the desired table declares it, or when ch waits for the plan's
// adds, which build every desired object the live table lacks. An object
// under the dropped name is never one: the drop clears that name first.
//
// The answer only names a keeper, so whatever it cannot prove gives no
// keeper rather than an error: a key on a column the plan retypes, whose
// equality the new type decides, and a key it cannot read, such as a
// temporal key the parser's grammar does not know.
func (h heldDrops) uniquenessKeeper(ch Change, waits, partitioned bool, desired Model) string {
	dropped, ok := h.droppedUniqueKey(ch)
	if !ok {
		return ""
	}
	type candidate struct {
		name string
		key  func() (statement.UniqueKey, error)
		kept bool
	}
	var candidates []candidate
	for _, con := range desired.Constraints {
		if con.Name == ch.Drops.Name || (!con.Primary && !isUniqueConstraint(con)) {
			continue
		}
		live, ok := h.liveCons[con.Name]
		candidates = append(candidates, candidate{
			name: con.Name,
			key:  func() (statement.UniqueKey, error) { return statement.ParseUniqueConstraint(con.Def) },
			kept: ok && !constraintNeedsDrop(live, h.desiredCons),
		})
	}
	for _, ix := range desired.Indexes {
		if ix.Name == ch.Drops.Name || !isUniqueIndex(ix) {
			continue
		}
		live, ok := h.liveIdx[ix.Name]
		candidates = append(candidates, candidate{
			name: ix.Name,
			key:  func() (statement.UniqueKey, error) { return statement.ParseUniqueIndex(ix.Def) },
			kept: ok && !live.Invalid && !indexNeedsDrop(partitioned, live, h.desiredIdx),
		})
	}
	slices.SortFunc(candidates, func(a, b candidate) int { return strings.Compare(a.name, b.name) })
	for _, c := range candidates {
		if !waits && !c.kept {
			continue
		}
		key, err := c.key()
		if err != nil || h.retypesAny(key) {
			continue
		}
		if key.Enforces(dropped) {
			return c.name
		}
	}
	return ""
}

// retypesAny reports whether the plan changes the type of a column key
// compares. The key's collation and operator class print empty when they
// are the type's default, so the same printed key compares values
// differently under the new type.
func (h heldDrops) retypesAny(key statement.UniqueKey) bool {
	for _, col := range key.Columns {
		if h.retyped[col.Column] {
			return true
		}
	}
	return false
}

// droppedUniqueKey returns the unique key ch drops when ch drops a live
// unique index or unique constraint whose key it can read.
func (h heldDrops) droppedUniqueKey(ch Change) (statement.UniqueKey, bool) {
	if ch.Drops == nil {
		return statement.UniqueKey{}, false
	}
	var key statement.UniqueKey
	var err error
	switch ch.Drops.Kind {
	case DroppedIndex:
		ix, ok := h.liveIdx[ch.Drops.Name]
		if !ok || !isUniqueIndex(ix) {
			return statement.UniqueKey{}, false
		}
		key, err = statement.ParseUniqueIndex(ix.Def)
	case DroppedConstraint:
		con, ok := h.liveCons[ch.Drops.Name]
		if !ok || !isUniqueConstraint(con) {
			return statement.UniqueKey{}, false
		}
		key, err = statement.ParseUniqueConstraint(con.Def)
	default:
		return statement.UniqueKey{}, false
	}
	if err != nil {
		return statement.UniqueKey{}, false
	}
	return key, true
}
