package statement

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseDesiredAdmitsTableAndIndexes(t *testing.T) {
	ds, err := ParseDesired(`create table events (
  id bigint primary key,
  name varchar(50) not null
);
create index events_name_idx on events (name);`)
	require.NoError(t, err)

	assert.Equal(t, "events", ds.Table)
	require.Len(t, ds.Statements, 2)
	assert.Equal(t, KindCreateTable, ds.Statements[0].Kind())
	assert.Equal(t, "CREATE TABLE events (id bigint PRIMARY KEY, name varchar(50) NOT NULL)", ds.Statements[0].SQL())
	assert.Equal(t, KindCreateIndex, ds.Statements[1].Kind())
	assert.Equal(t, "CREATE INDEX events_name_idx ON events USING btree (name)", ds.Statements[1].SQL())
}

func TestParseDesiredRefusals(t *testing.T) {
	tests := []struct {
		name    string
		sql     string
		wantErr error
	}{
		{"empty input", "", ErrEmptyDesired},
		{"comment only", "-- nothing here", ErrEmptyDesired},
		{"no create table", "CREATE INDEX i ON t (c)", ErrNoCreateTable},
		{"two create tables", "CREATE TABLE a (id int); CREATE TABLE b (id int)", ErrMultipleCreateTables},
		{"dml", "CREATE TABLE t (id int); DELETE FROM t", ErrDisallowedStatement},
		{"alter table", "CREATE TABLE t (id int); ALTER TABLE t ADD COLUMN c int", ErrDisallowedStatement},
		{"drop", "CREATE TABLE t (id int); DROP TABLE other", ErrDisallowedStatement},
		{"qualified table", "CREATE TABLE prod.t (id int)", ErrQualifiedName},
		{"qualified index", "CREATE TABLE t (id int); CREATE INDEX i ON prod.t (id)", ErrQualifiedName},
		{"concurrent index", "CREATE TABLE t (id int); CREATE INDEX CONCURRENTLY i ON t (id)", ErrConcurrentIndex},
		{"index on another table", "CREATE TABLE t (id int); CREATE INDEX i ON other (id)", ErrWrongIndexTarget},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseDesired(tt.sql)
			require.ErrorIs(t, err, tt.wantErr)
		})
	}
}

func TestParseDesiredRefusesInvalidSQL(t *testing.T) {
	_, err := ParseDesired("CREATE TABEL t (id int)")
	require.Error(t, err)
}

func TestQualify(t *testing.T) {
	got, err := Qualify("CREATE INDEX i ON t USING btree (c)", "s1")
	require.NoError(t, err)
	assert.Equal(t, "CREATE INDEX i ON s1.t USING btree (c)", got)

	got, err = Qualify("CREATE TABLE t (id int)", "s1")
	require.NoError(t, err)
	assert.Equal(t, "CREATE TABLE s1.t (id int)", got)
}

func TestQualifyEmptySchemaStripsQualification(t *testing.T) {
	got, err := Qualify("CREATE INDEX i ON s1.t USING btree (c)", "")
	require.NoError(t, err)
	assert.Equal(t, "CREATE INDEX i ON t USING btree (c)", got)
}

func TestQualifyRefusesOtherStatements(t *testing.T) {
	_, err := Qualify("ALTER TABLE t ADD COLUMN c int", "s1")
	require.ErrorIs(t, err, ErrDisallowedStatement)

	_, err = Qualify("CREATE TABLE a (id int); CREATE TABLE b (id int)", "s1")
	require.ErrorIs(t, err, ErrNotOneStatement)
}

func TestParseOneRecognizesCreateTable(t *testing.T) {
	st, err := ParseOne("CREATE TABLE prod.events (id int)")
	require.NoError(t, err)
	assert.Equal(t, KindCreateTable, st.Kind())
	assert.Equal(t, "prod", st.Schema())
	assert.Equal(t, "events", st.Table())
}
