package schemadiff

import (
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/statement"
)

// A standalone index whose definition changes under the same name is
// replaced, not dropped and rebuilt: the new index is built concurrently
// under a temporary name while the old one keeps serving reads and, for a
// unique index, keeps rejecting duplicates. Only once the new index is valid
// is the old one dropped, and the new one renamed to the old name. A unique
// rule is enforced by one index or the other at every moment, and a failed
// build leaves the old index in place.
//
// Dropping first, the way a removed index goes, would leave the table without
// the index for as long as the build takes, and a concurrent build waits for
// every older transaction in the database. Duplicates could land in that
// window and fail the build, leaving no unique rule at all.

// indexReplacement is how one changed index is replaced.
type indexReplacement struct {
	// live is the index the table has under the desired name, with the
	// old definition. Its zero value means an earlier run already dropped
	// it, after building temp.
	live Index
	// want is the desired index.
	want Index
	// temp is the temporary name the new index is built under.
	temp string
	// build is false when a valid index under temp with the desired
	// definition is already on the table: an earlier run built it and
	// stopped before the rename.
	build bool
}

// replacedIndexes names the standalone indexes of a table diff that are
// replaced rather than dropped and rebuilt, keyed by index name. A
// partitioned parent keeps the drop and rebuild, since its index is built
// partition by partition and never concurrently. So does a table whose diff
// drops or retypes a column: a dropped column takes its indexes with it,
// and a retype rebuilds them.
//
// A replacement an earlier run left part-way is picked up where it stopped:
// a valid temp index with the desired definition is not built again, and
// when the old index is already gone, only the rename is left.
func replacedIndexes(partitioned, dropsOrRetypesColumn bool, schema string, liveCons map[string]Constraint, liveIdx map[string]Index, desired Model, desiredIdx map[string]Index, adopted map[string]struct{}) (map[string]indexReplacement, error) {
	replaced := make(map[string]indexReplacement)
	if partitioned || dropsOrRetypesColumn {
		return replaced, nil
	}
	desiredCons := constraintsByName(desired.Constraints)
	for _, want := range desired.Indexes {
		temp := replacementIndexName(want.Name)
		had, ok := liveIdx[want.Name]
		switch {
		case ok && isIndexChange(had, want, adopted):
		case !ok:
			resumed, err := resumesRename(liveIdx, desiredIdx, want, temp)
			if err != nil {
				return nil, err
			}
			if resumed {
				replaced[want.Name] = indexReplacement{want: want, temp: temp}
			}
			continue
		default:
			continue
		}
		if err := refuseTakenReplacementName(want.Name, temp, desiredIdx, desiredCons, liveCons); err != nil {
			return nil, err
		}
		r := indexReplacement{live: had, want: want, temp: temp, build: true}
		if built, taken := liveIdx[temp]; taken {
			same, err := sameIndexUnderName(built, want)
			if err != nil {
				return nil, err
			}
			switch {
			case !same:
				return nil, replacementIndexHeld(schema, want.Name, built)
			case !built.Invalid:
				r.build = false
			}
		}
		replaced[want.Name] = r
	}
	return replaced, nil
}

// isIndexChange reports whether the live index had is a valid index that
// desired keeps under its name with another definition, and that no new key
// constraint takes over. An unfinished build is never replaced: its removal
// belongs to the concurrent build path (see indexNeedsDrop).
func isIndexChange(had, want Index, adopted map[string]struct{}) bool {
	if _, ok := adopted[had.Name]; ok {
		return false
	}
	return !had.Invalid && had.Def != want.Def
}

// resumesRename reports whether the table holds only the rename of an earlier
// replacement of want: want's name is free, and a valid index under temp
// with want's definition, which the desired table does not name, is on it.
func resumesRename(liveIdx, desiredIdx map[string]Index, want Index, temp string) (bool, error) {
	built, ok := liveIdx[temp]
	if !ok || built.Invalid {
		return false, nil
	}
	if _, named := desiredIdx[temp]; named {
		return false, nil
	}
	return sameIndexUnderName(built, want)
}

// refuseTakenReplacementName refuses a replacement whose temporary name the
// desired table names, or a live constraint holds. A live index under it is
// handled by the caller, which can tell an earlier run's build from another
// index.
func refuseTakenReplacementName(index, temp string, desiredIdx map[string]Index, desiredCons, liveCons map[string]Constraint) error {
	if _, taken := desiredIdx[temp]; taken {
		return unsupported(CauseReplacementIndexName, "%w: index %q is replaced by building its new definition as %q, which the desired table also names; rename that index",
			ErrUnsupportedChange, index, temp)
	}
	if _, taken := desiredCons[temp]; taken {
		return unsupported(CauseReplacementIndexName, "%w: index %q is replaced by building its new definition as %q, which the desired table also names; rename that constraint",
			ErrUnsupportedChange, index, temp)
	}
	if _, taken := liveCons[temp]; taken {
		return unsupported(CauseReplacementIndexName, "%w: index %q is replaced by building its new definition as %q, which the live table already names; rename that constraint",
			ErrUnsupportedChange, index, temp)
	}
	return nil
}

// replacementIndexHeld refuses a replacement whose temporary name a live
// index with another definition holds. An invalid one is most likely the
// failed build of an earlier attempt at another definition; the refusal
// names the statement that clears it, since no plan can tell an abandoned
// build from one still running. A valid one is an index of the table's own,
// which the replacement must not drop and reuse the name of.
func replacementIndexHeld(schema, index string, ix Index) error {
	if ix.Invalid {
		return unsupported(CauseReplacementIndexName, "%w: index %q is replaced by building its new definition as %q, and an unfinished build of another definition is on the table under that name, likely left by an earlier attempt; once pg_stat_progress_create_index shows no backend building it, run DROP INDEX CONCURRENTLY %s and plan again",
			ErrUnsupportedChange, index, ix.Name, pgx.Identifier{schema, ix.Name}.Sanitize())
	}
	return unsupported(CauseReplacementIndexName, "%w: index %q is replaced by building its new definition as %q, which a live index on the table already names; rename that index",
		ErrUnsupportedChange, index, ix.Name)
}

// sameIndexUnderName reports whether built, read under its own name, has
// want's definition. Both are renamed to want's name and printed by the same
// deparser, so the comparison sees the definition and not the name.
func sameIndexUnderName(built, want Index) (bool, error) {
	got, err := statement.RenameIndex(built.Def, want.Name)
	if err != nil {
		return false, fmt.Errorf("read the definition of index %s: %w", built.Name, err)
	}
	wanted, err := statement.RenameIndex(want.Def, want.Name)
	if err != nil {
		return false, fmt.Errorf("read the definition of index %s: %w", want.Name, err)
	}
	return got == wanted, nil
}

// replaceIndexChanges is the plan that replaces r.live with r.want: build
// the new index under its temporary name, drop the old index, then rename
// the new one to the old name. Each step a resumed replacement has already
// taken is left out.
func replaceIndexChanges(schema string, r indexReplacement) ([]Change, error) {
	var changes []Change
	if r.build {
		def, err := statement.RenameIndex(r.want.Def, r.temp)
		if err != nil {
			return nil, fmt.Errorf("name the replacement of index %s: %w", r.want.Name, err)
		}
		create, err := statement.Qualify(def, schema)
		if err != nil {
			return nil, fmt.Errorf("qualify the replacement of index %s: %w", r.want.Name, err)
		}
		changes = append(changes, Change{SQL: create, Kind: ChangeCreateIndex})
	}
	if r.live.Name != "" {
		changes = append(changes, Change{
			SQL:         "DROP INDEX " + pgx.Identifier{schema, r.live.Name}.Sanitize(),
			Kind:        ChangeDropIndex,
			Destructive: true,
			Drops:       &DroppedObject{Kind: DroppedIndex, Name: r.live.Name, Definition: r.live.Def},
			ReplacedBy:  r.temp,
		})
	}
	changes = append(changes, Change{
		SQL:  "ALTER INDEX " + pgx.Identifier{schema, r.temp}.Sanitize() + " RENAME TO " + pgx.Identifier{r.want.Name}.Sanitize(),
		Kind: ChangeRenameIndex,
	})
	return changes, nil
}
