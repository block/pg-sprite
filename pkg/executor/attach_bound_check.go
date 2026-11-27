package executor

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/block/pg-sprite/pkg/statement"
)

// The bound proofs an online ATTACH PARTITION stands on. A plain ATTACH
// scans the attached table to prove its rows fit the bound, and the parent's
// DEFAULT partition to prove none of its rows belongs to the bound, each
// under ACCESS EXCLUSIVE. PostgreSQL skips a scan when a validated CHECK on
// the table already implies the proof (predicate_implied_by), so the flow
// adds those CHECKs NOT VALID and validates them online first. This file
// renders the two CHECK expressions from the parent's partition key and the
// statement's bound. It renders them statically: probing the bound with a
// CREATE TABLE ... PARTITION OF would hold ACCESS EXCLUSIVE on the parent.
//
// The server decides the implication, so a CHECK rendered here can only
// cost a missed proof, never a wrong attach: a CHECK that does not imply the
// bound leaves the scan in place. Each form mirrors the partition constraint
// the server derives (partbounds.c, get_qual_for_range, get_qual_for_list,
// get_qual_for_hash) so the proof succeeds: an explicit IS NOT NULL per key,
// a multi-column range expanded into the server's OR arms rather than a row
// comparison, which the implication proof does not understand, and the key's
// own operator-class operators with every value cast to the key's type.

// errAttachBoundUnprovable is returned for a bound or partition key the
// derivation cannot turn into a CHECK the server would accept as proof. The
// flow refuses it before it adds anything.
var errAttachBoundUnprovable = errors.New("the partition bound cannot be proven ahead of the attach")

// attachKeyColumn is one column of the parent's partition key, rendered.
type attachKeyColumn struct {
	// expr is the key as SQL: a quoted column name, or the key expression
	// in parentheses, as the server printed it.
	expr string
	// typeSQL is the type the bound's values are cast to, as format_type
	// printed it.
	typeSQL string
	// collatable is whether the key's type has a collation.
	collatable bool
	// The operator class's comparison operators, each as OPERATOR(...).
	// Unset for a hash key.
	lt, le, eq, ge, gt string
}

// attachKey is the parent's partition key, as the derivation needs it.
type attachKey struct {
	strategy statement.PartitionStrategy
	// parentLiteral is the parent's qualified name as a quoted SQL literal,
	// which a hash bound's satisfies_hash_partition names it by.
	parentLiteral string
	columns       []attachKeyColumn
}

// boundValue is one value of the bound, resolved against its key column.
// literal is set only for a statement.BoundValue: the value cast to the key's
// type, as SQL.
type boundValue struct {
	kind    statement.BoundDatumKind
	literal string
}

// attachBound is the statement's bound with every value resolved.
type attachBound struct {
	strategy     statement.PartitionStrategy
	lower, upper []boundValue
	// equalPrefix counts the leading range columns whose lower and upper
	// values are equal under the key's equality operator. The server states
	// each as key = value rather than as OR arms.
	equalPrefix        int
	list               []boundValue
	modulus, remainder int
}

// attachBoundCheck renders the two proofs of an attach: check, the CHECK
// expression on the attached table that implies its partition constraint,
// and defaultExclusion, the one on the DEFAULT partition that excludes the
// new bound, empty for a hash bound, whose parent has no DEFAULT partition.
// defaultExclusion is NOT of the bound without its IS NOT NULL terms: a row
// with a NULL key stays in the DEFAULT partition unless the new bound lists
// NULL.
func attachBoundCheck(key attachKey, bound attachBound) (check, defaultExclusion string, err error) {
	if bound.strategy != key.strategy {
		return "", "", fmt.Errorf("%w: the bound is a %s bound and the table is partitioned by %s",
			errAttachBoundUnprovable, bound.strategy, key.strategy)
	}
	switch key.strategy {
	case statement.PartitionByRange:
		return rangeBoundCheck(key, bound)
	case statement.PartitionByList:
		return listBoundCheck(key, bound)
	case statement.PartitionByHash:
		check, err := hashBoundCheck(key, bound)
		return check, "", err
	default:
		return "", "", fmt.Errorf("%w: unknown partition strategy %q", ErrInvariantViolation, key.strategy)
	}
}

// rangeBoundCheck mirrors get_qual_for_range: IS NOT NULL on every key
// column, key = value for the equal prefix, then the lower and the upper
// bound each as OR arms over the remaining columns.
func rangeBoundCheck(key attachKey, bound attachBound) (string, string, error) {
	n := len(key.columns)
	if len(bound.lower) != n || len(bound.upper) != n {
		return "", "", fmt.Errorf("%w: the range bound has %d and %d values for a %d-column key",
			errAttachBoundUnprovable, len(bound.lower), len(bound.upper), n)
	}
	if err := checkRangeSide("FROM", bound.lower); err != nil {
		return "", "", err
	}
	if err := checkRangeSide("TO", bound.upper); err != nil {
		return "", "", err
	}
	if bound.equalPrefix < 0 || bound.equalPrefix >= n {
		return "", "", fmt.Errorf("%w: the range bound's lower and upper values are equal on %d of %d columns, an empty range",
			errAttachBoundUnprovable, bound.equalPrefix, n)
	}
	var terms []string
	for i := range bound.equalPrefix {
		c := key.columns[i]
		terms = append(terms, comparison(c, c.eq, bound.lower[i]))
	}
	if lower := rangeArms(key.columns, bound.lower, bound.equalPrefix, true); lower != "" {
		terms = append(terms, lower)
	}
	if upper := rangeArms(key.columns, bound.upper, bound.equalPrefix, false); upper != "" {
		terms = append(terms, upper)
	}
	if len(terms) == 0 {
		return "", "", fmt.Errorf("%w: the range bound is unbounded on both sides, so only the IS NOT NULL of its key would describe it",
			errAttachBoundUnprovable)
	}
	return strings.Join(append(notNullTerms(key.columns), terms...), " AND "), negation(terms), nil
}

// checkRangeSide refuses a side the server would not accept as written:
// NULL, an expression, or a value after MINVALUE or MAXVALUE.
func checkRangeSide(side string, values []boundValue) error {
	infinite := false
	for _, v := range values {
		switch v.kind {
		case statement.BoundMinValue, statement.BoundMaxValue:
			infinite = true
		case statement.BoundValue:
			if infinite {
				return fmt.Errorf("%w: the %s bound has a value after MINVALUE or MAXVALUE", errAttachBoundUnprovable, side)
			}
		default:
			return unprovableValue(v)
		}
	}
	if values[0].kind == statement.BoundMaxValue && side == "FROM" {
		return fmt.Errorf("%w: the range starts at MAXVALUE, an empty range", errAttachBoundUnprovable)
	}
	if values[0].kind == statement.BoundMinValue && side == "TO" {
		return fmt.Errorf("%w: the range ends at MINVALUE, an empty range", errAttachBoundUnprovable)
	}
	return nil
}

// rangeArms is one side of the range as the server writes it: arm k holds
// key = value on the first k remaining columns and the side's comparison on
// the next one. An arm's last comparison is inclusive (>= or <=) where the
// bound includes its edge: a lower bound on its last column or before
// MINVALUE, an upper bound before MAXVALUE. The arms stop at the first
// MINVALUE or MAXVALUE, past which the side bounds nothing.
func rangeArms(columns []attachKeyColumn, side []boundValue, start int, lower bool) string {
	var arms []string
	for last := start; last < len(columns) && side[last].kind == statement.BoundValue; last++ {
		var args []string
		for j := start; j < last; j++ {
			args = append(args, comparison(columns[j], columns[j].eq, side[j]))
		}
		args = append(args, comparison(columns[last], rangeOperator(columns, side, last, lower), side[last]))
		arms = append(arms, conjunction(args))
	}
	switch len(arms) {
	case 0:
		return ""
	case 1:
		return arms[0]
	default:
		return "(" + strings.Join(arms, " OR ") + ")"
	}
}

// rangeOperator is the comparison of an arm's last column.
func rangeOperator(columns []attachKeyColumn, side []boundValue, j int, lower bool) string {
	var next statement.BoundDatumKind
	if j+1 < len(side) {
		next = side[j+1].kind
	}
	c := columns[j]
	if lower {
		if j == len(columns)-1 || next == statement.BoundMinValue {
			return c.ge
		}
		return c.gt
	}
	if next == statement.BoundMaxValue {
		return c.le
	}
	return c.lt
}

// listBoundCheck mirrors get_qual_for_list: key = ANY of the listed values,
// with IS NULL OR'd in when NULL is listed and IS NOT NULL ANDed in when it
// is not.
func listBoundCheck(key attachKey, bound attachBound) (string, string, error) {
	if len(key.columns) != 1 {
		return "", "", fmt.Errorf("%w: a list partition key has %d columns", ErrInvariantViolation, len(key.columns))
	}
	c := key.columns[0]
	var literals []string
	hasNull := false
	for _, v := range bound.list {
		switch v.kind {
		case statement.BoundValue:
			literals = append(literals, v.literal)
		case statement.BoundNull:
			hasNull = true
		default:
			return "", "", unprovableValue(v)
		}
	}
	// The implication proof expands a constant array into one comparison
	// per element only up to a limit (MAX_SAOP_ARRAY_SIZE in predtest.c).
	if len(literals) > maxProvableListValues {
		return "", "", fmt.Errorf("%w: the list bound has %d values, more than the %d the server's implication proof expands",
			errAttachBoundUnprovable, len(literals), maxProvableListValues)
	}
	var in string
	if len(literals) > 0 {
		in = c.expr + " " + c.eq + " ANY (ARRAY[" + strings.Join(literals, ", ") + "])"
	}
	switch {
	case hasNull && in == "":
		return c.expr + " IS NULL", "NOT (" + c.expr + " IS NULL)", nil
	case hasNull:
		predicate := "(" + c.expr + " IS NULL OR " + in + ")"
		return predicate, "NOT " + predicate, nil
	case in == "":
		return "", "", fmt.Errorf("%w: the list bound lists no value", errAttachBoundUnprovable)
	default:
		return c.expr + " IS NOT NULL AND " + in, "NOT (" + in + ")", nil
	}
}

// maxProvableListValues is predtest.c's MAX_SAOP_ARRAY_SIZE.
const maxProvableListValues = 100

// hashBoundCheck mirrors get_qual_for_hash: satisfies_hash_partition of the
// parent, the modulus, the remainder, and every key column. The server's
// call carries no input collation, and the parser gives a collatable
// argument one, so the two never compare equal and a collatable hash key is
// refused.
func hashBoundCheck(key attachKey, bound attachBound) (string, error) {
	if bound.modulus <= 0 || bound.remainder < 0 || bound.remainder >= bound.modulus {
		return "", fmt.Errorf("%w: hash bound MODULUS %d, REMAINDER %d", errAttachBoundUnprovable, bound.modulus, bound.remainder)
	}
	args := []string{
		"CAST(" + key.parentLiteral + " AS pg_catalog.regclass)",
		strconv.Itoa(bound.modulus),
		strconv.Itoa(bound.remainder),
	}
	for _, c := range key.columns {
		if c.collatable {
			return "", fmt.Errorf("%w: hash key %s has a collatable type, and the server's hash partition constraint never matches a CHECK over it",
				errAttachBoundUnprovable, c.expr)
		}
		args = append(args, c.expr)
	}
	return "pg_catalog.satisfies_hash_partition(" + strings.Join(args, ", ") + ")", nil
}

func notNullTerms(columns []attachKeyColumn) []string {
	terms := make([]string, 0, len(columns))
	for _, c := range columns {
		terms = append(terms, c.expr+" IS NOT NULL")
	}
	return terms
}

func comparison(c attachKeyColumn, operator string, v boundValue) string {
	return c.expr + " " + operator + " " + v.literal
}

func conjunction(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	return "(" + strings.Join(args, " AND ") + ")"
}

func negation(terms []string) string {
	return "NOT (" + strings.Join(terms, " AND ") + ")"
}

func unprovableValue(v boundValue) error {
	switch v.kind {
	case statement.BoundNull:
		return fmt.Errorf("%w: a range bound cannot hold NULL", errAttachBoundUnprovable)
	case statement.BoundExpression:
		return fmt.Errorf("%w: a bound value is an expression, not a plain constant, so its value is not fixed until the attach evaluates it", errAttachBoundUnprovable)
	case statement.BoundMinValue, statement.BoundMaxValue:
		return fmt.Errorf("%w: a list bound cannot hold MINVALUE or MAXVALUE", errAttachBoundUnprovable)
	default:
		return fmt.Errorf("%w: unknown bound value kind %d", ErrInvariantViolation, v.kind)
	}
}
