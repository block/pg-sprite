package statement

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A table file that holds several statements the front door refuses names
// every one of them, in file order and at the line it starts on, so its
// author fixes them in one edit instead of one parse at a time.
func TestParseDesiredFileNamesEveryRefusedStatement(t *testing.T) {
	sql := `CREATE TABLE orders (
  id bigint PRIMARY KEY,
  updated_at timestamptz NOT NULL
);

COMMENT ON TABLE orders IS 'Orders placed by accounts';

CREATE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
  NEW.updated_at := now();
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER orders_updated_at BEFORE UPDATE ON orders
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();
`
	_, err := ParseDesiredFile(sql)
	require.Error(t, err)

	refused, ok := errors.AsType[*RefusedStatementsError](err)
	require.True(t, ok, "the error lists the refused statements: %v", err)
	require.Len(t, refused.Refusals, 3)

	want := []struct {
		statement, line int
		sql             string
		kind            Kind
	}{
		{2, 6, "COMMENT ON TABLE orders IS 'Orders placed by accounts'", KindCatalogWork},
		{3, 8, "CREATE FUNCTION set_updated_at()", KindCatalogWork},
		{4, 15, "CREATE TRIGGER orders_updated_at BEFORE UPDATE ON orders", KindCatalogWork},
	}
	for i, w := range want {
		got := refused.Refusals[i]
		assert.Equal(t, w.statement, got.Statement, "refusal %d", i)
		assert.Equal(t, w.line, got.Line, "refusal %d", i)
		assert.Equal(t, 1, got.Column, "refusal %d", i)
		assert.ErrorIs(t, got, ErrDisallowedStatement, "refusal %d", i)
		disallowed, ok := errors.AsType[*DisallowedStatementError](got.Err)
		require.True(t, ok, "refusal %d names its statement", i)
		assert.Contains(t, disallowed.SQL, w.sql, "refusal %d", i)
		assert.Equal(t, w.kind, disallowed.Kind, "refusal %d", i)
	}

	t.Run("it still reads as a disallowed statement", func(t *testing.T) {
		assert.ErrorIs(t, err, ErrDisallowedStatement)
		disallowed, ok := errors.AsType[*DisallowedStatementError](err)
		require.True(t, ok)
		assert.Contains(t, disallowed.SQL, "COMMENT ON TABLE orders", "the first refusal answers a single-error caller")
	})
}

// A file with one refused statement reads exactly as it did before refusals
// were collected: the statement's position, then why.
func TestParseDesiredFileOneRefusalReadsAsOneError(t *testing.T) {
	_, err := ParseDesiredFile("CREATE TABLE t (id int);\nDELETE FROM t;\n")
	require.Error(t, err)
	assert.Equal(t, "statement 2: statement kind not allowed in a desired schema: DELETE FROM t", err.Error())

	refused, ok := errors.AsType[*RefusedStatementsError](err)
	require.True(t, ok)
	require.Len(t, refused.Refusals, 1)
	assert.Equal(t, 2, refused.Refusals[0].Line)
}

// Refusals of different kinds in one file are all named, in file order,
// including an index on another table that the file declares before the
// table it belongs with, and a second CREATE TABLE that is a partition.
func TestParseDesiredFileOrdersMixedRefusals(t *testing.T) {
	sql := `CREATE INDEX accounts_email ON accounts (email);
CREATE TABLE events (
  id bigint NOT NULL,
  at timestamptz NOT NULL
) PARTITION BY RANGE (at);
CREATE TABLE events_2026_10 PARTITION OF events
  FOR VALUES FROM ('2026-10-01') TO ('2026-11-01');
GRANT SELECT ON events TO reader;
`
	_, err := ParseDesiredFile(sql)
	require.Error(t, err)
	refused, ok := errors.AsType[*RefusedStatementsError](err)
	require.True(t, ok, "%v", err)
	require.Len(t, refused.Refusals, 3)

	assert.Equal(t, 1, refused.Refusals[0].Statement)
	assert.Equal(t, 1, refused.Refusals[0].Line)
	assert.ErrorIs(t, refused.Refusals[0], ErrWrongIndexTarget)

	assert.Equal(t, 3, refused.Refusals[1].Statement)
	assert.Equal(t, 6, refused.Refusals[1].Line)
	assert.ErrorIs(t, refused.Refusals[1], ErrMultipleCreateTables)
	assert.ErrorIs(t, refused.Refusals[1], ErrDesiredPartitions)

	assert.Equal(t, 4, refused.Refusals[2].Statement)
	assert.Equal(t, 8, refused.Refusals[2].Line)
	assert.ErrorIs(t, refused.Refusals[2], ErrDisallowedStatement)
}

// An enum type file names every statement beside its CREATE TYPE, not only
// the first.
func TestParseDesiredFileEnumNamesEveryExtraStatement(t *testing.T) {
	sql := `CREATE TYPE order_status AS ENUM ('placed', 'shipped');
COMMENT ON TYPE order_status IS 'Where an order is';
CREATE TYPE order_status_old AS ENUM ('placed');
`
	_, err := ParseDesiredFile(sql)
	require.Error(t, err)
	refused, ok := errors.AsType[*RefusedStatementsError](err)
	require.True(t, ok, "%v", err)
	require.Len(t, refused.Refusals, 2)
	assert.Equal(t, []int{2, 3}, []int{refused.Refusals[0].Line, refused.Refusals[1].Line})
	for _, refusal := range refused.Refusals {
		assert.ErrorIs(t, refusal, ErrEnumTypeAlone)
	}
}

// A row-security file's table and indexes are admitted apart from its row
// security, yet a refusal of one of them still names its own statement and
// line in the file, not its place among the table's statements.
func TestParseDesiredFileRowSecurityRefusalNamesItsPlaceInTheFile(t *testing.T) {
	sql := `CREATE TABLE accounts (id bigint PRIMARY KEY, owner text NOT NULL);
ALTER TABLE accounts ENABLE ROW LEVEL SECURITY;
CREATE INDEX orders_owner ON orders (owner);
`
	_, err := ParseDesiredFile(sql)
	require.Error(t, err)
	refused, ok := errors.AsType[*RefusedStatementsError](err)
	require.True(t, ok, "%v", err)
	require.Len(t, refused.Refusals, 1)
	got := refused.Refusals[0]
	assert.Equal(t, 3, got.Statement)
	assert.Equal(t, 3, got.Line)
	assert.Equal(t, 1, got.Column)
	assert.ErrorIs(t, got, ErrWrongIndexTarget)
}
