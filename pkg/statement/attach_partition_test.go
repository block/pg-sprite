package statement_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/statement"
)

// A range bound reads as the grammar parsed it: the tables as spelled, and
// each value a constant, MINVALUE, or MAXVALUE.
func TestAttachedPartitionReadsARangeBound(t *testing.T) {
	got, ok, err := statement.AttachedPartition(`
		ALTER TABLE app.events
			ATTACH PARTITION app.events_2027_01
			FOR VALUES FROM (MINVALUE, 5) TO ('2027-02-01', MAXVALUE)`)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, statement.AttachPartition{
		ParentSchema: "app", Parent: "events", Schema: "app", Name: "events_2027_01",
		Bound: statement.PartitionBound{
			Strategy: statement.PartitionByRange,
			Lower:    []statement.BoundDatum{{Kind: statement.BoundMinValue}, {Kind: statement.BoundValue, Value: "5"}},
			Upper:    []statement.BoundDatum{{Kind: statement.BoundValue, Value: "2027-02-01"}, {Kind: statement.BoundMaxValue}},
		},
	}, got)
}

// A list bound keeps NULL apart from the constants, and a negative number,
// a decimal, and a boolean keep the text the grammar read; a cast or a
// function call is an expression whose value the statement does not fix.
func TestAttachedPartitionReadsListValues(t *testing.T) {
	got, ok, err := statement.AttachedPartition(`
		ALTER TABLE events
			ATTACH PARTITION events_misc
			FOR VALUES IN (NULL, -3, 1.50, true, 'eu', 'us'::text, lower('X'))`)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, []statement.BoundDatum{
		{Kind: statement.BoundNull},
		{Kind: statement.BoundValue, Value: "-3"},
		{Kind: statement.BoundValue, Value: "1.50"},
		{Kind: statement.BoundValue, Value: "true"},
		{Kind: statement.BoundValue, Value: "eu"},
		{Kind: statement.BoundExpression},
		{Kind: statement.BoundExpression},
	}, got.Bound.List)
	assert.Equal(t, statement.PartitionByList, got.Bound.Strategy)
	assert.Empty(t, got.ParentSchema)
}

func TestAttachedPartitionReadsHashAndDefaultBounds(t *testing.T) {
	got, ok, err := statement.AttachedPartition(`
		ALTER TABLE app.accounts
			ATTACH PARTITION app.accounts_p3
			FOR VALUES WITH (MODULUS 8, REMAINDER 3)`)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, statement.PartitionBound{Strategy: statement.PartitionByHash, Modulus: 8, Remainder: 3}, got.Bound)

	got, ok, err = statement.AttachedPartition(`ALTER TABLE app.events ATTACH PARTITION app.events_default DEFAULT`)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, statement.PartitionBound{Default: true}, got.Bound)
}

func TestAttachedPartitionIgnoresOtherStatements(t *testing.T) {
	for _, sql := range []string{
		`ALTER TABLE app.events DETACH PARTITION app.events_2024`,
		`ALTER TABLE app.events ADD COLUMN note text`,
		`CREATE TABLE app.events_2027_01 PARTITION OF app.events FOR VALUES FROM ('2027-01-01') TO ('2027-02-01')`,
	} {
		_, ok, err := statement.AttachedPartition(sql)
		require.NoError(t, err, sql)
		assert.False(t, ok, sql)
	}
}

// A partition key as pg_get_partkeydef prints it: columns by name, an
// expression by its printed text, and an explicit COLLATE or operator class
// flagged.
func TestPartitionKeyReadsColumnsAndExpressions(t *testing.T) {
	strategy, elems, err := statement.PartitionKey(`RANGE (tenant_id, "Day", ((a + b)), lower(name), date_trunc('day'::text, ts))`)
	require.NoError(t, err)
	assert.Equal(t, statement.PartitionByRange, strategy)
	assert.Equal(t, []statement.PartitionKeyElem{
		{Column: "tenant_id"},
		{Column: "Day"},
		{Expression: "((a + b))"},
		{Expression: "lower(name)"},
		{Expression: "date_trunc('day'::text, ts)"},
	}, elems)

	strategy, elems, err = statement.PartitionKey(`LIST (region COLLATE "C" text_pattern_ops)`)
	require.NoError(t, err)
	assert.Equal(t, statement.PartitionByList, strategy)
	assert.Equal(t, []statement.PartitionKeyElem{{Column: "region", Collated: true, OpClass: true}}, elems)
}
