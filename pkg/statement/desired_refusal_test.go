package statement

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A statement a desired file may not hold is refused with the statement and
// the kind ParseOne gives it, through either front door, so a caller can
// classify the refusal as the imperative door classifies the same statement.
func TestParseDesiredFileNamesTheDisallowedStatementAndItsKind(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		stmt string
		kind Kind
	}{
		{"enum type file", "CREATE TYPE mood AS ENUM ('happy', 'sad')",
			"CREATE TYPE mood AS ENUM ('happy', 'sad')", KindCatalogWork},
		{"domain beside a table", "CREATE TABLE t (id int); CREATE DOMAIN positive AS int CHECK (VALUE > 0)",
			"CREATE DOMAIN positive AS int CHECK (value > 0)", KindCatalogWork},
		{"data change", "CREATE TABLE t (id int); DELETE FROM t", "DELETE FROM t", KindDataChange},
		{"grant", "CREATE TABLE t (id int); GRANT SELECT ON t TO reader", "GRANT select ON t TO reader", KindProvisioning},
		{"alter table", "CREATE TABLE t (id int); ALTER TABLE t ADD COLUMN c int",
			"ALTER TABLE t ADD COLUMN c int", KindAlterTable},
		{"drop index", "CREATE TABLE t (id int); DROP INDEX i", "DROP INDEX i", KindDropIndex},
		{"enum type beside row security",
			"CREATE TABLE t (id int); ALTER TABLE t ENABLE ROW LEVEL SECURITY; CREATE TYPE mood AS ENUM ('happy')",
			"CREATE TYPE mood AS ENUM ('happy')", KindCatalogWork},
		{"comment on a table beside row security",
			"CREATE TABLE t (id int); CREATE POLICY p ON t USING (true); COMMENT ON TABLE t IS 'people'",
			"COMMENT ON TABLE t IS 'people'", KindCatalogWork},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseDesiredFile(tt.sql)
			require.ErrorIs(t, err, ErrDisallowedStatement)
			disallowed, ok := errors.AsType[*DisallowedStatementError](err)
			require.True(t, ok, "the refusal carries the statement: %v", err)
			assert.Equal(t, tt.stmt, disallowed.SQL)
			assert.Equal(t, tt.kind, disallowed.Kind)
		})
	}
}

// A second CREATE TABLE is refused with its text, and as partitions when
// either table is one: a file of PARTITION OF tables, or a partitioned
// parent with a partition beside it.
func TestParseDesiredFileNamesTheSecondTable(t *testing.T) {
	tests := []struct {
		name      string
		sql       string
		stmt      string
		partition bool
	}{
		{"two unrelated tables", "CREATE TABLE a (id int); CREATE TABLE b (id int)",
			"CREATE TABLE b (id int)", false},
		{"partitions of a table",
			"CREATE TABLE log_p1 PARTITION OF log FOR VALUES FROM (1) TO (2); " +
				"CREATE TABLE log_p2 PARTITION OF log FOR VALUES FROM (2) TO (3)",
			"CREATE TABLE log_p2 PARTITION OF log FOR VALUES FROM (2) TO (3)", true},
		{"parent and a partition",
			"CREATE TABLE log (id int) PARTITION BY RANGE (id); CREATE TABLE log_p1 PARTITION OF log DEFAULT",
			"CREATE TABLE log_p1 PARTITION OF log DEFAULT", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseDesiredFile(tt.sql)
			require.ErrorIs(t, err, ErrMultipleCreateTables)
			second, ok := errors.AsType[*SecondTableError](err)
			require.True(t, ok, "the refusal carries the table: %v", err)
			assert.Equal(t, tt.stmt, second.SQL)
			assert.Equal(t, tt.partition, second.Partition)
			assert.Equal(t, tt.partition, errors.Is(err, ErrDesiredPartitions))
		})
	}
}
