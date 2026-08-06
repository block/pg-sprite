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
			want: Statement{Kind: KindAlterTable, Table: "users"},
		},
		{
			name: "alter table schema-qualified",
			sql:  "ALTER TABLE billing.invoices DROP COLUMN note",
			want: Statement{Kind: KindAlterTable, Schema: "billing", Table: "invoices"},
		},
		{
			name: "alter table quoted mixed-case identifier",
			sql:  `ALTER TABLE "Order Items" ADD COLUMN qty int`,
			want: Statement{Kind: KindAlterTable, Table: "Order Items"},
		},
		{
			name: "alter table if exists",
			sql:  "ALTER TABLE IF EXISTS users ADD COLUMN age int",
			want: Statement{Kind: KindAlterTable, Table: "users"},
		},
		{
			name: "create index",
			sql:  "CREATE INDEX idx_users_email ON users (email)",
			want: Statement{Kind: KindCreateIndex, Table: "users"},
		},
		{
			name: "create index schema-qualified",
			sql:  "CREATE INDEX idx_users_email ON billing.users (email)",
			want: Statement{Kind: KindCreateIndex, Schema: "billing", Table: "users"},
		},
		{
			name: "create unique index concurrently is still an index statement",
			sql:  "CREATE UNIQUE INDEX CONCURRENTLY idx ON users (email)",
			want: Statement{Kind: KindCreateIndex, Table: "users"},
		},
		{
			name: "drop index",
			sql:  "DROP INDEX idx_users_email",
			want: Statement{Kind: KindDropIndex},
		},
		{
			name: "reindex table",
			sql:  "REINDEX TABLE users",
			want: Statement{Kind: KindReindex},
		},
		{
			name: "reindex index",
			sql:  "REINDEX INDEX idx_users_email",
			want: Statement{Kind: KindReindex},
		},
		{
			name: "alter index parses as AlterTableStmt but is not a table target",
			sql:  "ALTER INDEX idx_users_email SET (fillfactor = 90)",
			want: Statement{Kind: KindOther},
		},
		{
			name: "drop table is not a drop-index",
			sql:  "DROP TABLE users",
			want: Statement{Kind: KindOther},
		},
		{
			name: "create table",
			sql:  "CREATE TABLE t (id int)",
			want: Statement{Kind: KindCreateTable, Table: "t"},
		},
		{
			name: "dml",
			sql:  "UPDATE users SET age = 1",
			want: Statement{Kind: KindOther},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseOne(tt.sql)
			require.NoError(t, err)
			tt.want.SQL = tt.sql
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
