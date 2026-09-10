package statement

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseOneKinds(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want Statement
	}{
		{
			name: "alter table unqualified",
			sql:  "ALTER TABLE users ADD COLUMN age int",
			want: Statement{kind: KindAlterTable, table: "users"},
		},
		{
			name: "alter table schema-qualified",
			sql:  "ALTER TABLE billing.invoices DROP COLUMN note",
			want: Statement{kind: KindAlterTable, schema: "billing", table: "invoices"},
		},
		{
			name: "alter table quoted mixed-case identifier",
			sql:  `ALTER TABLE "Order Items" ADD COLUMN qty int`,
			want: Statement{kind: KindAlterTable, table: "Order Items"},
		},
		{
			name: "alter table if exists",
			sql:  "ALTER TABLE IF EXISTS users ADD COLUMN age int",
			want: Statement{kind: KindAlterTable, table: "users"},
		},
		{
			name: "alter table rename to parses as RenameStmt but is a table target",
			sql:  "ALTER TABLE users RENAME TO users_old",
			want: Statement{kind: KindAlterTable, table: "users"},
		},
		{
			name: "alter table rename column",
			sql:  "ALTER TABLE users RENAME COLUMN a TO b",
			want: Statement{kind: KindAlterTable, table: "users"},
		},
		{
			name: "alter table rename constraint",
			sql:  "ALTER TABLE users RENAME CONSTRAINT users_pk TO users_pkey",
			want: Statement{kind: KindAlterTable, table: "users"},
		},
		{
			name: "alter table rename schema-qualified",
			sql:  "ALTER TABLE billing.users RENAME COLUMN a TO b",
			want: Statement{kind: KindAlterTable, schema: "billing", table: "users"},
		},
		{
			name: "alter table set schema parses as AlterObjectSchemaStmt but is a table target",
			sql:  "ALTER TABLE users SET SCHEMA archive",
			want: Statement{kind: KindAlterTable, table: "users"},
		},
		{
			name: "alter table owner to",
			sql:  "ALTER TABLE users OWNER TO app_owner",
			want: Statement{kind: KindAlterTable, table: "users"},
		},
		{
			name: "alter view rename is not a table target",
			sql:  "ALTER VIEW v RENAME TO w",
			want: Statement{kind: KindOther},
		},
		{
			name: "alter index rename is not a table target",
			sql:  "ALTER INDEX i RENAME TO j",
			want: Statement{kind: KindOther},
		},
		{
			name: "alter sequence set schema is not a table target",
			sql:  "ALTER SEQUENCE s SET SCHEMA archive",
			want: Statement{kind: KindOther},
		},
		{
			name: "create index",
			sql:  "CREATE INDEX idx_users_email ON users (email)",
			want: Statement{kind: KindCreateIndex, table: "users", buildsIndex: true},
		},
		{
			name: "create index schema-qualified",
			sql:  "CREATE INDEX idx_users_email ON billing.users (email)",
			want: Statement{kind: KindCreateIndex, schema: "billing", table: "users", buildsIndex: true},
		},
		{
			name: "create unique index concurrently is a concurrent index statement",
			sql:  "CREATE UNIQUE INDEX CONCURRENTLY idx ON users (email)",
			want: Statement{kind: KindCreateIndex, table: "users", concurrent: true, buildsIndex: true},
		},
		{
			name: "drop index",
			sql:  "DROP INDEX idx_users_email",
			want: Statement{kind: KindDropIndex},
		},
		{
			name: "drop index concurrently",
			sql:  "DROP INDEX CONCURRENTLY idx_users_email",
			want: Statement{kind: KindDropIndex, concurrent: true},
		},
		{
			name: "reindex table",
			sql:  "REINDEX TABLE users",
			want: Statement{kind: KindReindex},
		},
		{
			name: "reindex index",
			sql:  "REINDEX INDEX idx_users_email",
			want: Statement{kind: KindReindex},
		},
		{
			name: "reindex table concurrently",
			sql:  "REINDEX TABLE CONCURRENTLY users",
			want: Statement{kind: KindReindex, concurrent: true},
		},
		{
			name: "alter index parses as AlterTableStmt but is not a table target",
			sql:  "ALTER INDEX idx_users_email SET (fillfactor = 90)",
			want: Statement{kind: KindOther},
		},
		{
			name: "drop table is not a drop-index",
			sql:  "DROP TABLE users",
			want: Statement{kind: KindCatalogWork},
		},
		{
			name: "create table",
			sql:  "CREATE TABLE t (id int)",
			want: Statement{kind: KindCreateTable, table: "t"},
		},
		{
			name: "dml",
			sql:  "UPDATE users SET age = 1",
			want: Statement{kind: KindDataChange},
		},
		{
			name: "grant is provisioning, unlike create role catalog syntax",
			sql:  "GRANT SELECT ON users TO app",
			want: Statement{kind: KindProvisioning},
		},
		{
			name: "insert is data change, unlike create table as",
			sql:  "INSERT INTO users VALUES (1)",
			want: Statement{kind: KindDataChange},
		},
		{
			name: "create table as is neither a plain create table nor a data change",
			sql:  "CREATE TABLE users_copy AS SELECT * FROM users",
			want: Statement{kind: KindOther},
		},
		{
			name: "delete is data change",
			sql:  "DELETE FROM users WHERE id = 1",
			want: Statement{kind: KindDataChange},
		},
		{
			name: "merge is data change",
			sql:  "MERGE INTO users u USING staged s ON u.id = s.id WHEN MATCHED THEN UPDATE SET age = s.age",
			want: Statement{kind: KindDataChange},
		},
		{
			name: "create role is provisioning",
			sql:  "CREATE ROLE app LOGIN",
			want: Statement{kind: KindProvisioning},
		},
		{
			name: "row-level-security policy is provisioning",
			sql:  "CREATE POLICY p ON users USING (owner = current_user)",
			want: Statement{kind: KindProvisioning},
		},
		{
			name: "publication is provisioning",
			sql:  "CREATE PUBLICATION pub FOR TABLE users",
			want: Statement{kind: KindProvisioning},
		},
		{
			name: "create view is catalog work",
			sql:  "CREATE VIEW v AS SELECT id FROM users",
			want: Statement{kind: KindCatalogWork},
		},
		{
			name: "create function is catalog work",
			sql:  "CREATE FUNCTION f() RETURNS int LANGUAGE sql AS 'SELECT 1'",
			want: Statement{kind: KindCatalogWork},
		},
		{
			name: "create trigger is catalog work",
			sql:  "CREATE TRIGGER trg BEFORE INSERT ON users FOR EACH ROW EXECUTE FUNCTION f()",
			want: Statement{kind: KindCatalogWork},
		},
		{
			name: "create extension is catalog work",
			sql:  "CREATE EXTENSION pg_stat_statements",
			want: Statement{kind: KindCatalogWork},
		},
		{
			name: "standalone sequence is catalog work",
			sql:  "CREATE SEQUENCE users_id_seq",
			want: Statement{kind: KindCatalogWork},
		},
		{
			name: "comment is catalog work",
			sql:  "COMMENT ON TABLE users IS 'people'",
			want: Statement{kind: KindCatalogWork},
		},
		{
			name: "drop view is catalog work, unlike drop index",
			sql:  "DROP VIEW v",
			want: Statement{kind: KindCatalogWork},
		},
		{
			name: "vacuum is unnamed grammar and stays other",
			sql:  "VACUUM users",
			want: Statement{kind: KindOther},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseOne(tt.sql)
			require.NoError(t, err)
			tt.want.sql = tt.sql
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseOneRejectsInvalidSQL(t *testing.T) {
	_, err := ParseOne("ALTER TABEL users ADD COLUMN age int")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse statement")
}

func TestParseOneRejectsMultipleStatements(t *testing.T) {
	_, err := ParseOne("ALTER TABLE a ADD COLUMN x int; ALTER TABLE b ADD COLUMN y int")
	require.ErrorIs(t, err, ErrNotOneStatement)
}

// A second statement smuggled behind a legitimate ALTER never yields a
// Statement at all — the executor only accepts what ParseOne constructs, so
// multi-statement SQL is unrepresentable downstream (invariant ST-7).
func TestParseOneRejectsSmuggledStatement(t *testing.T) {
	_, err := ParseOne("ALTER TABLE t ADD COLUMN a int; DROP TABLE victim")
	require.ErrorIs(t, err, ErrNotOneStatement)
}

func TestParseOneRejectsEmptyInput(t *testing.T) {
	_, err := ParseOne("")
	require.ErrorIs(t, err, ErrNotOneStatement)
}

func TestKindString(t *testing.T) {
	assert.Equal(t, "ALTER TABLE", KindAlterTable.String())
	assert.Equal(t, "CREATE INDEX", KindCreateIndex.String())
	assert.Equal(t, "DROP INDEX", KindDropIndex.String())
	assert.Equal(t, "REINDEX", KindReindex.String())
	assert.Equal(t, "other", KindOther.String())
}

// Canonical is the report's one rendering per change: the diff door's
// quoted generation and the alter door's hand-written text converge on the
// same string, so a consumer hashing or displaying report SQL sees one
// spelling regardless of front door.
func TestCanonicalConvergesQuotingAcrossFrontDoors(t *testing.T) {
	generated, err := Canonical(`ALTER TABLE "t_1"."t" DROP COLUMN "doomed"`)
	require.NoError(t, err)
	submitted, err := Canonical("ALTER TABLE t_1.t DROP COLUMN doomed")
	require.NoError(t, err)
	assert.Equal(t, generated, submitted)

	// Identifiers that need quoting keep it.
	kept, err := Canonical(`ALTER TABLE "Mixed Case" DROP COLUMN c`)
	require.NoError(t, err)
	assert.Contains(t, kept, `"Mixed Case"`)
}

func TestCanonicalRefusesNotExactlyOneStatement(t *testing.T) {
	_, err := Canonical("ALTER TABLE t DROP COLUMN a; ALTER TABLE t DROP COLUMN b")
	require.ErrorIs(t, err, ErrNotOneStatement)
	_, err = Canonical("not sql")
	require.Error(t, err)
}

// Reprinting through the deparser drops comments, so Canonical refuses
// commented input rather than silently discarding content.
func TestCanonicalRefusesCommentedInput(t *testing.T) {
	_, err := Canonical("ALTER TABLE t DROP COLUMN a -- doomed")
	require.ErrorIs(t, err, ErrCommentLoss)
	_, err = Canonical("ALTER TABLE t /* keep */ DROP COLUMN a")
	require.ErrorIs(t, err, ErrCommentLoss)
}

func TestBuildsIndex(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want bool
	}{
		{"create index", "CREATE INDEX i ON s.t (c)", true},
		{"create index concurrently", "CREATE INDEX CONCURRENTLY i ON s.t (c)", true},
		{"add unique constraint", "ALTER TABLE s.t ADD CONSTRAINT u UNIQUE (c)", true},
		{"add primary key", "ALTER TABLE s.t ADD CONSTRAINT p PRIMARY KEY (c)", true},
		{"add exclusion constraint", "ALTER TABLE s.t ADD CONSTRAINT x EXCLUDE USING gist (c WITH =)", true},
		{"add column with inline unique", "ALTER TABLE s.t ADD COLUMN e int UNIQUE", true},
		{"add column with inline primary key", "ALTER TABLE s.t ADD COLUMN e int PRIMARY KEY", true},
		{"unique using index adopts an existing index", "ALTER TABLE s.t ADD CONSTRAINT u UNIQUE USING INDEX u", false},
		{"primary key using index adopts an existing index", "ALTER TABLE s.t ADD CONSTRAINT p PRIMARY KEY USING INDEX p", false},
		{"plain add column", "ALTER TABLE s.t ADD COLUMN e int", false},
		{"check constraint builds nothing", "ALTER TABLE s.t ADD CONSTRAINT c CHECK (e > 0)", false},
		{"foreign key builds nothing on the referencing table", "ALTER TABLE s.t ADD CONSTRAINT fk FOREIGN KEY (e) REFERENCES s.p (id)", false},
		{"rewriting type change rebuilds existing indexes only", "ALTER TABLE s.t ALTER COLUMN c TYPE bigint", false},
		{"set not null", "ALTER TABLE s.t ALTER COLUMN c SET NOT NULL", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, err := ParseOne(tt.sql)
			require.NoError(t, err)
			assert.Equal(t, tt.want, st.BuildsIndex())
		})
	}
}

func TestKindsIsClosedAndNamed(t *testing.T) {
	kinds := Kinds()
	require.Equal(t, KindOther, kinds[0], "the catch-all leads the walk")
	seen := map[Kind]bool{}
	names := map[string]bool{}
	for _, k := range kinds {
		assert.False(t, seen[k], "duplicate kind %d", k)
		seen[k] = true
		assert.NotEmpty(t, k.String())
		assert.False(t, names[k.String()], "duplicate kind name %q", k.String())
		names[k.String()] = true
	}
	assert.Len(t, kinds, int(KindCatalogWork)+1, "Kinds() must list every declared kind")
}
