package executor

import (
	"context"
	"fmt"

	"github.com/block/pg-sprite/pkg/statement"
)

// How the attach step ties the reviewed statement's bound to what it acts
// on: the proofs it is given, and the bound the catalog records for a table
// already attached. Both are judged with the bound derivation's own reads
// (readAttachKey, resolveAttachBound), so the statement's bound means the
// same thing here as it did when its proofs were rendered.

// requireProofsOfBound reads the parent's partition key, resolves the
// statement's bound against it, and requires the target's proofs to be the
// two CHECKs the bound derives (attachBoundCheck). It returns the key and the
// resolved bound.
func requireProofsOfBound(ctx context.Context, q querier, t attachPartitionTarget, bound statement.PartitionBound) (attachKey, attachBound, error) {
	key, err := readAttachKey(ctx, q, t.parentOID)
	if err != nil {
		return attachKey{}, attachBound{}, err
	}
	reviewed, err := resolveAttachBound(ctx, q, key, bound)
	if err != nil {
		return attachKey{}, attachBound{}, err
	}
	check, exclusion, err := attachBoundCheck(key, reviewed)
	if err != nil {
		return attachKey{}, attachBound{}, err
	}
	if err := t.requireProofsOf(check, exclusion); err != nil {
		return attachKey{}, attachBound{}, err
	}
	return key, reviewed, nil
}

// requireProofsOf requires the table's proof to carry check, and the DEFAULT
// partition's, when the run proved one, to carry exclusion.
func (t attachPartitionTarget) requireProofsOf(check, exclusion string) error {
	if t.bound.expr != check {
		return fmt.Errorf("%w: the proof on %s is (%s), and the statement's bound derives (%s)",
			errAttachProofsNotOfTheBound, t.bound.qualified(), t.bound.expr, check)
	}
	if t.hasDefault() && t.deflt.expr != exclusion {
		return fmt.Errorf("%w: the proof on the DEFAULT partition %s is (%s), and the statement's bound derives (%s)",
			errAttachProofsNotOfTheBound, t.deflt.qualified(), t.deflt.expr, exclusion)
	}
	return nil
}

// requireAttachedUnder requires a table that is already a partition of the
// parent to be attached under the reviewed bound. The catalog's bound, as
// the server prints it, is parsed and resolved against the key like the
// statement's, then compared value by value with the key's own equality
// operator: the server prints a value in its type's output form, which the
// statement need not have used, so `'2027-01-01'` for a timestamp key prints
// as `'2027-01-01 00:00:00'` and the two texts, or the CHECKs rendered from
// them, differ while the bounds are the same.
func requireAttachedUnder(ctx context.Context, q querier, key attachKey, reviewed attachBound, p attachProof, printed string) error {
	parsed, err := statement.PrintedPartitionBound(printed)
	if err != nil {
		return err
	}
	if parsed.Default {
		return fmt.Errorf("%w: %s is the parent's DEFAULT partition", errAttachedUnderAnotherBound, p.qualified())
	}
	catalog, err := resolveAttachBound(ctx, q, key, parsed)
	if err != nil {
		return err
	}
	same, err := sameAttachBound(ctx, q, key, reviewed, catalog)
	if err != nil {
		return err
	}
	if !same {
		return fmt.Errorf("%w: %s is attached to the parent %s, not under the reviewed statement's bound",
			errAttachedUnderAnotherBound, p.qualified(), printed)
	}
	return nil
}

// sameAttachBound reports whether two bounds resolved against the same key
// are the same bound: a range's lower and upper values pairwise equal, a
// list's values equal as sets, a hash bound's modulus and remainder equal.
func sameAttachBound(ctx context.Context, q querier, key attachKey, a, b attachBound) (bool, error) {
	if a.strategy != b.strategy {
		return false, nil
	}
	switch a.strategy {
	case statement.PartitionByRange:
		same, err := sameBoundValues(ctx, q, key.columns, a.lower, b.lower)
		if err != nil {
			return false, err
		}
		if !same {
			return false, nil
		}
		return sameBoundValues(ctx, q, key.columns, a.upper, b.upper)
	case statement.PartitionByList:
		covered, err := listCovers(ctx, q, key.columns[0], a.list, b.list)
		if err != nil {
			return false, err
		}
		if !covered {
			return false, nil
		}
		return listCovers(ctx, q, key.columns[0], b.list, a.list)
	case statement.PartitionByHash:
		return a.modulus == b.modulus && a.remainder == b.remainder, nil
	default:
		return false, fmt.Errorf("%w: unknown partition strategy %q", ErrInvariantViolation, a.strategy)
	}
}

// sameBoundValues reports whether two range sides hold equal values, one per
// key column.
func sameBoundValues(ctx context.Context, q querier, columns []attachKeyColumn, a, b []boundValue) (bool, error) {
	if len(a) != len(columns) || len(b) != len(columns) {
		return false, nil
	}
	for i, c := range columns {
		equal, err := boundValueEqual(ctx, q, c, a[i], b[i])
		if err != nil {
			return false, err
		}
		if !equal {
			return false, nil
		}
	}
	return true, nil
}

// listCovers reports whether every value of a has an equal value in b.
func listCovers(ctx context.Context, q querier, c attachKeyColumn, a, b []boundValue) (bool, error) {
	for _, x := range a {
		found := false
		for _, y := range b {
			equal, err := boundValueEqual(ctx, q, c, x, y)
			if err != nil {
				return false, err
			}
			if equal {
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
	}
	return true, nil
}

// boundValueEqual reports whether two values of one key column are equal:
// MINVALUE, MAXVALUE, and NULL each equal only themselves, and two constants
// are compared by the server under the key's equality operator, each the
// server-quoted literal cast to the key's type (resolveValues).
func boundValueEqual(ctx context.Context, q querier, c attachKeyColumn, x, y boundValue) (bool, error) {
	if x.kind != y.kind {
		return false, nil
	}
	if x.kind != statement.BoundValue {
		return true, nil
	}
	var equal bool
	if err := q.QueryRow(ctx, "SELECT "+x.literal+" "+c.eq+" "+y.literal).Scan(&equal); err != nil {
		return false, fmt.Errorf("compare the bound values %s and %s: %w", x.literal, y.literal, err)
	}
	return equal, nil
}
