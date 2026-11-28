package executor

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/statement"
)

// btreeColumn is a key column with the pg_catalog comparison operators, as
// readAttachKey renders them for a built-in type.
func btreeColumn(expr, typeSQL string) attachKeyColumn {
	op := func(name string) string { return `OPERATOR("pg_catalog".` + name + `)` }
	return attachKeyColumn{expr: expr, typeSQL: typeSQL, lt: op("<"), le: op("<="), eq: op("="), ge: op(">="), gt: op(">")}
}

func value(literal string) boundValue {
	return boundValue{kind: statement.BoundValue, literal: literal}
}

func listValues(n int) []boundValue {
	values := make([]boundValue, 0, n)
	for i := range n {
		values = append(values, value(fmt.Sprintf("CAST('v%d' AS text)", i)))
	}
	return values
}

var (
	minValue = boundValue{kind: statement.BoundMinValue}
	maxValue = boundValue{kind: statement.BoundMaxValue}
	nullItem = boundValue{kind: statement.BoundNull}
)

// FOR VALUES FROM ('2027-01-01') TO ('2027-02-01') on a date key: the key
// is not null, at least the lower value, and below the upper one.
func TestAttachBoundCheckSingleColumnRange(t *testing.T) {
	key := attachKey{strategy: statement.PartitionByRange, columns: []attachKeyColumn{btreeColumn(`"day"`, "date")}}
	check, exclusion, err := attachBoundCheck(key, attachBound{
		strategy: statement.PartitionByRange,
		lower:    []boundValue{value(`CAST('2027-01-01' AS date)`)},
		upper:    []boundValue{value(`CAST('2027-02-01' AS date)`)},
	})
	require.NoError(t, err)
	assert.Equal(t, `"day" IS NOT NULL`+
		` AND "day" OPERATOR("pg_catalog".>=) CAST('2027-01-01' AS date)`+
		` AND "day" OPERATOR("pg_catalog".<) CAST('2027-02-01' AS date)`, check)
	assert.Equal(t, `NOT ("day" IS NOT NULL`+
		` AND "day" OPERATOR("pg_catalog".>=) CAST('2027-01-01' AS date)`+
		` AND "day" OPERATOR("pg_catalog".<) CAST('2027-02-01' AS date))`, exclusion)
}

// FOR VALUES FROM (1, 10) TO (2, 20) on (a, b): each side as the server's OR
// arms, since the implication proof does not understand (a, b) >= (1, 10).
// The DEFAULT exclusion negates the whole check, IS NOT NULL terms
// included, as the server's DEFAULT constraint does: a row with b NULL
// belongs in the DEFAULT partition even where its a alone satisfies the
// arms.
func TestAttachBoundCheckMultiColumnRangeExpandsArms(t *testing.T) {
	key := attachKey{strategy: statement.PartitionByRange, columns: []attachKeyColumn{btreeColumn(`"a"`, "integer"), btreeColumn(`"b"`, "integer")}}
	check, exclusion, err := attachBoundCheck(key, attachBound{
		strategy: statement.PartitionByRange,
		lower:    []boundValue{value("1"), value("10")},
		upper:    []boundValue{value("2"), value("20")},
	})
	require.NoError(t, err)
	assert.Equal(t, `"a" IS NOT NULL AND "b" IS NOT NULL`+
		` AND ("a" OPERATOR("pg_catalog".>) 1 OR ("a" OPERATOR("pg_catalog".=) 1 AND "b" OPERATOR("pg_catalog".>=) 10))`+
		` AND ("a" OPERATOR("pg_catalog".<) 2 OR ("a" OPERATOR("pg_catalog".=) 2 AND "b" OPERATOR("pg_catalog".<) 20))`, check)
	assert.Equal(t, `NOT ("a" IS NOT NULL AND "b" IS NOT NULL`+
		` AND ("a" OPERATOR("pg_catalog".>) 1 OR ("a" OPERATOR("pg_catalog".=) 1 AND "b" OPERATOR("pg_catalog".>=) 10))`+
		` AND ("a" OPERATOR("pg_catalog".<) 2 OR ("a" OPERATOR("pg_catalog".=) 2 AND "b" OPERATOR("pg_catalog".<) 20)))`, exclusion)
}

// FOR VALUES FROM (5, 10) TO (5, 20): the server states the equal first
// column as a = 5, and the OR arms start after it.
func TestAttachBoundCheckRangeEqualPrefix(t *testing.T) {
	key := attachKey{strategy: statement.PartitionByRange, columns: []attachKeyColumn{btreeColumn(`"a"`, "integer"), btreeColumn(`"b"`, "integer")}}
	check, _, err := attachBoundCheck(key, attachBound{
		strategy:    statement.PartitionByRange,
		lower:       []boundValue{value("5"), value("10")},
		upper:       []boundValue{value("5"), value("20")},
		equalPrefix: 1,
	})
	require.NoError(t, err)
	assert.Equal(t, `"a" IS NOT NULL AND "b" IS NOT NULL`+
		` AND "a" OPERATOR("pg_catalog".=) 5`+
		` AND "b" OPERATOR("pg_catalog".>=) 10`+
		` AND "b" OPERATOR("pg_catalog".<) 20`, check)
}

// FROM (1, MINVALUE) TO (2, MAXVALUE) is a >= 1 AND a <= 2; FROM (1,
// MAXVALUE) is a > 1, and TO (2, MINVALUE) is a < 2. A side that starts
// with MINVALUE or MAXVALUE bounds nothing.
func TestAttachBoundCheckRangeInfiniteValues(t *testing.T) {
	key := attachKey{strategy: statement.PartitionByRange, columns: []attachKeyColumn{btreeColumn(`"a"`, "integer"), btreeColumn(`"b"`, "integer")}}
	tests := []struct {
		name         string
		lower, upper []boundValue
		want         string
	}{
		{"inclusive both sides", []boundValue{value("1"), minValue}, []boundValue{value("2"), maxValue},
			`"a" IS NOT NULL AND "b" IS NOT NULL AND "a" OPERATOR("pg_catalog".>=) 1 AND "a" OPERATOR("pg_catalog".<=) 2`},
		{"exclusive both sides", []boundValue{value("1"), maxValue}, []boundValue{value("2"), minValue},
			`"a" IS NOT NULL AND "b" IS NOT NULL AND "a" OPERATOR("pg_catalog".>) 1 AND "a" OPERATOR("pg_catalog".<) 2`},
		{"no lower side", []boundValue{minValue, minValue}, []boundValue{value("2"), value("5")},
			`"a" IS NOT NULL AND "b" IS NOT NULL` +
				` AND ("a" OPERATOR("pg_catalog".<) 2 OR ("a" OPERATOR("pg_catalog".=) 2 AND "b" OPERATOR("pg_catalog".<) 5))`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			check, _, err := attachBoundCheck(key, attachBound{strategy: statement.PartitionByRange, lower: tt.lower, upper: tt.upper})
			require.NoError(t, err)
			assert.Equal(t, tt.want, check)
		})
	}
}

// FOR VALUES IN ('eu', 'us'): not null and one of the values. With NULL
// listed, IS NULL is OR'd in instead, and the DEFAULT partition must then
// hold no NULL either.
func TestAttachBoundCheckList(t *testing.T) {
	key := attachKey{strategy: statement.PartitionByList, columns: []attachKeyColumn{btreeColumn(`"region"`, "text")}}
	check, exclusion, err := attachBoundCheck(key, attachBound{
		strategy: statement.PartitionByList,
		list:     []boundValue{value(`CAST('eu' AS text)`), value(`CAST('us' AS text)`)},
	})
	require.NoError(t, err)
	assert.Equal(t, `"region" IS NOT NULL AND "region" OPERATOR("pg_catalog".=) ANY (ARRAY[CAST('eu' AS text), CAST('us' AS text)])`, check)
	assert.Equal(t, `NOT ("region" OPERATOR("pg_catalog".=) ANY (ARRAY[CAST('eu' AS text), CAST('us' AS text)]))`, exclusion)

	check, exclusion, err = attachBoundCheck(key, attachBound{
		strategy: statement.PartitionByList,
		list:     []boundValue{nullItem, value(`CAST('eu' AS text)`)},
	})
	require.NoError(t, err)
	assert.Equal(t, `("region" IS NULL OR "region" OPERATOR("pg_catalog".=) ANY (ARRAY[CAST('eu' AS text)]))`, check)
	assert.Equal(t, `NOT ("region" IS NULL OR "region" OPERATOR("pg_catalog".=) ANY (ARRAY[CAST('eu' AS text)]))`, exclusion)

	check, exclusion, err = attachBoundCheck(key, attachBound{strategy: statement.PartitionByList, list: []boundValue{nullItem}})
	require.NoError(t, err)
	assert.Equal(t, `"region" IS NULL`, check)
	assert.Equal(t, `NOT ("region" IS NULL)`, exclusion)
}

// FOR VALUES WITH (MODULUS 4, REMAINDER 1): the server's own hash test over
// the parent and every key column; a hash parent has no DEFAULT partition.
func TestAttachBoundCheckHash(t *testing.T) {
	key := attachKey{
		strategy:      statement.PartitionByHash,
		parentLiteral: `'app.accounts'`,
		columns:       []attachKeyColumn{{expr: `"id"`, typeSQL: "bigint"}, {expr: `("a" + "b")`, typeSQL: "integer"}},
	}
	check, exclusion, err := attachBoundCheck(key, attachBound{strategy: statement.PartitionByHash, modulus: 4, remainder: 1})
	require.NoError(t, err)
	assert.Equal(t, `pg_catalog.satisfies_hash_partition(CAST('app.accounts' AS pg_catalog.regclass), 4, 1, "id", ("a" + "b"))`, check)
	assert.Empty(t, exclusion)
}

// Shapes the server would not accept as proof, or that do not fix the bound
// until the attach evaluates it, are refused before anything is rendered.
func TestAttachBoundCheckRefusesUnprovableShapes(t *testing.T) {
	rangeKey := attachKey{strategy: statement.PartitionByRange, columns: []attachKeyColumn{btreeColumn(`"a"`, "integer"), btreeColumn(`"b"`, "integer")}}
	listKey := attachKey{strategy: statement.PartitionByList, columns: []attachKeyColumn{btreeColumn(`"region"`, "text")}}
	relabeledColumn := btreeColumn(`"region"`, "character varying(20)")
	relabeledColumn.relabeled = true
	relabeledListKey := attachKey{strategy: statement.PartitionByList, columns: []attachKeyColumn{relabeledColumn}}
	hashKey := attachKey{strategy: statement.PartitionByHash, parentLiteral: `'app.t'`, columns: []attachKeyColumn{{expr: `"name"`, typeSQL: "text", collatable: true}}}
	intHashKey := attachKey{strategy: statement.PartitionByHash, parentLiteral: `'app.t'`, columns: []attachKeyColumn{{expr: `"id"`, typeSQL: "bigint"}}}
	expression := boundValue{kind: statement.BoundExpression}
	tests := []struct {
		name  string
		key   attachKey
		bound attachBound
	}{
		// A modulus and remainder that would render a valid hash check,
		// carried on a range bound: only the strategy guard refuses it.
		{"strategy mismatch", intHashKey, attachBound{strategy: statement.PartitionByRange, modulus: 2, remainder: 0}},
		{"wrong arity", rangeKey, attachBound{strategy: statement.PartitionByRange, lower: []boundValue{value("1")}, upper: []boundValue{value("2")}}},
		{"expression value", rangeKey, attachBound{strategy: statement.PartitionByRange, lower: []boundValue{expression, value("1")}, upper: []boundValue{value("2"), value("2")}}},
		{"value after MINVALUE", rangeKey, attachBound{strategy: statement.PartitionByRange, lower: []boundValue{minValue, value("1")}, upper: []boundValue{value("2"), value("2")}}},
		{"unbounded both sides", rangeKey, attachBound{strategy: statement.PartitionByRange, lower: []boundValue{minValue, minValue}, upper: []boundValue{maxValue, maxValue}}},
		{"all columns equal", rangeKey, attachBound{strategy: statement.PartitionByRange, lower: []boundValue{value("1"), value("1")}, upper: []boundValue{value("1"), value("1")}, equalPrefix: 2}},
		{"list expression", listKey, attachBound{strategy: statement.PartitionByList, list: []boundValue{expression}}},
		{"over 100 values on a relabeled key", relabeledListKey, attachBound{strategy: statement.PartitionByList, list: listValues(101)}},
		{"range starts at MAXVALUE", rangeKey, attachBound{strategy: statement.PartitionByRange, lower: []boundValue{maxValue, maxValue}, upper: []boundValue{maxValue, maxValue}}},
		{"range ends at MINVALUE", rangeKey, attachBound{strategy: statement.PartitionByRange, lower: []boundValue{minValue, minValue}, upper: []boundValue{minValue, minValue}}},
		{"collatable hash key", hashKey, attachBound{strategy: statement.PartitionByHash, modulus: 2, remainder: 0}},
		{"hash MODULUS 0", intHashKey, attachBound{strategy: statement.PartitionByHash, modulus: 0, remainder: 0}},
		{"hash REMAINDER below 0", intHashKey, attachBound{strategy: statement.PartitionByHash, modulus: 2, remainder: -1}},
		{"hash REMAINDER not below MODULUS", intHashKey, attachBound{strategy: statement.PartitionByHash, modulus: 2, remainder: 2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := attachBoundCheck(tt.key, tt.bound)
			require.ErrorIs(t, err, errAttachBoundUnprovable)
		})
	}
}

// An operator name is spelled bare inside OPERATOR(), so one with a
// character outside the operator set is refused rather than rendered.
func TestOperatorSQLRefusesNonOperatorNames(t *testing.T) {
	got, err := operatorSQL("app schema", "<=")
	require.NoError(t, err)
	assert.Equal(t, `OPERATOR("app schema".<=)`, got)
	_, err = operatorSQL("pg_catalog", "<) OR true OR (")
	require.ErrorIs(t, err, ErrInvariantViolation)
}
