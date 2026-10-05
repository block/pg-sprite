package schemachange

import "math"

// PostgreSQL's type OIDs for the three integer types an identity column
// may have; pg_sequence.seqtypid and pg_attribute.atttypid carry them.
const (
	int2TypeOID uint32 = 21
	int4TypeOID uint32 = 23
	int8TypeOID uint32 = 20
)

// integerTypeBounds returns the smallest and largest value of one of the
// integer types an identity column may have, and false for any other type.
func integerTypeBounds(typeOID uint32) (minValue, maxValue int64, ok bool) {
	switch typeOID {
	case int2TypeOID:
		return math.MinInt16, math.MaxInt16, true
	case int4TypeOID:
		return math.MinInt32, math.MaxInt32, true
	case int8TypeOID:
		return math.MinInt64, math.MaxInt64, true
	default:
		return 0, 0, false
	}
}

// rebaseSequenceBounds moves the declared bounds of a sequence of type from
// to a sequence of type to, the way the server's own ALTER SEQUENCE … AS
// does when ALTER COLUMN … TYPE retypes an identity column: a bound that
// equals the old type's own limit was never chosen by anyone and follows
// the new type; a bound that differs was declared and stays as written.
// Each bound is judged on its own. Types that are not integer types leave
// the options untouched.
func rebaseSequenceBounds(opts SequenceOptions, from, to uint32) SequenceOptions {
	fromMin, fromMax, fromOK := integerTypeBounds(from)
	toMin, toMax, toOK := integerTypeBounds(to)
	if !fromOK || !toOK {
		return opts
	}
	if opts.Min == fromMin {
		opts.Min = toMin
	}
	if opts.Max == fromMax {
		opts.Max = toMax
	}
	return opts
}
