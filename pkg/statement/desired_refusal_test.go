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
		{"domain file", "CREATE DOMAIN email AS text", "CREATE DOMAIN email AS text", KindCatalogWork},
		{"composite type file", "CREATE TYPE pair AS (a int, b int)", "CREATE TYPE pair AS (a int, b int)", KindCatalogWork},
		{"domain beside a table", "CREATE TABLE t (id int); CREATE DOMAIN positive AS int CHECK (VALUE > 0)",
			"CREATE DOMAIN positive AS int CHECK (value > 0)", KindCatalogWork},
		{"data change", "CREATE TABLE t (id int); DELETE FROM t", "DELETE FROM t", KindDataChange},
		{"grant", "CREATE TABLE t (id int); GRANT SELECT ON t TO reader", "GRANT select ON t TO reader", KindProvisioning},
		{"alter table", "CREATE TABLE t (id int); ALTER TABLE t ADD COLUMN c int",
			"ALTER TABLE t ADD COLUMN c int", KindAlterTable},
		{"drop index", "CREATE TABLE t (id int); DROP INDEX i", "DROP INDEX i", KindDropIndex},
		{"domain beside row security",
			"CREATE TABLE t (id int); ALTER TABLE t ENABLE ROW LEVEL SECURITY; CREATE DOMAIN email AS text",
			"CREATE DOMAIN email AS text", KindCatalogWork},
		{"comment on a table beside row security",
			"CREATE TABLE t (id int); CREATE POLICY p ON t USING (true); COMMENT ON TABLE t IS 'people'",
			"COMMENT ON TABLE t IS 'people'", KindCatalogWork},
		// The parse tree spells an empty enum label as no label, so these
		// deparse as a plain ADD VALUE. The refusal names and classifies the
		// statement the file holds.
		{"renaming the empty enum label",
			"CREATE TABLE t (id int);\nALTER TYPE mood RENAME VALUE '' TO 'blue';\n",
			"ALTER TYPE mood RENAME VALUE '' TO 'blue'", KindOther},
		{"adding an enum label before the empty label",
			"CREATE TABLE t (id int);\nALTER TYPE mood ADD VALUE 'calm' BEFORE '';\n",
			"ALTER TYPE mood ADD VALUE 'calm' BEFORE ''", KindOther},
		{"renaming the empty enum label beside row security",
			"CREATE TABLE t (id int); ALTER TABLE t ENABLE ROW LEVEL SECURITY; ALTER TYPE mood RENAME VALUE '' TO 'blue'",
			"ALTER TYPE mood RENAME VALUE '' TO 'blue'", KindOther},
		{"adding an enum label",
			"CREATE TABLE t (id int); alter type mood add value 'calm'",
			"ALTER TYPE mood ADD VALUE 'calm'", KindAddEnumValue},
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

// A refused statement names the object it declares, changes, or annotates,
// finer than its kind: a comment, a trigger, and a domain share a kind, and
// each belongs somewhere different instead of the desired file.
func TestParseDesiredFileNamesTheObjectARefusedStatementDeclares(t *testing.T) {
	const table = "CREATE TABLE t (id int);\n"
	tests := []struct {
		name   string
		sql    string
		object DeclaredObject
	}{
		{"comment", table + "COMMENT ON TABLE t IS 'people'", DeclaredComment},
		{"comment on a column", table + "COMMENT ON COLUMN t.id IS 'key'", DeclaredComment},
		{"trigger", table + "CREATE TRIGGER audit AFTER INSERT ON t FOR EACH ROW EXECUTE FUNCTION audit()", DeclaredTrigger},
		{"function", table + "CREATE FUNCTION audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$", DeclaredFunction},
		{"procedure", table + "CREATE PROCEDURE p() LANGUAGE sql AS $$ SELECT 1 $$", DeclaredFunction},
		{"view", table + "CREATE VIEW v AS SELECT id FROM t", DeclaredView},
		{"materialized view", table + "CREATE MATERIALIZED VIEW mv AS SELECT id FROM t", DeclaredMaterializedView},
		{"materialized view alone", "CREATE MATERIALIZED VIEW mv AS SELECT 1 AS id", DeclaredMaterializedView},
		{"table as select", table + "CREATE TABLE t2 AS SELECT id FROM t", DeclaredOther},
		{"domain", "CREATE DOMAIN email AS text", DeclaredDomain},
		{"statistics", table + "CREATE STATISTICS s ON id, id FROM t", DeclaredStatistics},
		{"sequence", table + "CREATE SEQUENCE seq", DeclaredSequence},
		{"composite type", "CREATE TYPE pair AS (a int, b int)", DeclaredType},
		{"range type", table + "CREATE TYPE floatrange AS RANGE (subtype = float8)", DeclaredType},
		{"extension", table + "CREATE EXTENSION citext", DeclaredExtension},
		{"grant", table + "GRANT SELECT ON t TO reader", DeclaredGrant},
		{"revoke", table + "REVOKE SELECT ON t FROM reader", DeclaredGrant},
		{"role grant", table + "GRANT reader TO app", DeclaredGrant},
		{"schema", table + "CREATE SCHEMA audit", DeclaredSchema},
		{"rule", table + "CREATE RULE r AS ON INSERT TO t DO INSTEAD NOTHING", DeclaredRule},
		{"data change", table + "DELETE FROM t", DeclaredOther},
		{"comment beside row security",
			table + "CREATE POLICY p ON t USING (true); COMMENT ON TABLE t IS 'people'", DeclaredComment},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseDesiredFile(tt.sql)
			disallowed, ok := errors.AsType[*DisallowedStatementError](err)
			require.True(t, ok, "the refusal carries the statement: %v", err)
			assert.Equal(t, tt.object, disallowed.Object)
		})
	}
}

// Every declared object has a noun of its own, so a message that names one
// never reads as another.
func TestDeclaredObjectsHaveDistinctNames(t *testing.T) {
	seen := map[string]DeclaredObject{}
	for _, object := range DeclaredObjects() {
		name := object.String()
		require.NotEmpty(t, name)
		other, dup := seen[name]
		require.False(t, dup, "objects %d and %d share the name %q", other, object, name)
		seen[name] = object
	}
	assert.Equal(t, "statement", DeclaredOther.String())
}
