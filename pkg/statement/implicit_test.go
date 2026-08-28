package statement_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/statement"
)

func TestImplicitIndexNames(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want []string
	}{
		{
			name: "no index-backed constraints",
			sql:  "CREATE TABLE t (id int, name text, CHECK (id > 0))",
			want: nil,
		},
		{
			name: "inline primary key",
			sql:  "CREATE TABLE t (id int PRIMARY KEY)",
			want: []string{"t_pkey"},
		},
		{
			name: "table primary key",
			sql:  "CREATE TABLE t (id int, PRIMARY KEY (id))",
			want: []string{"t_pkey"},
		},
		{
			name: "named table primary key",
			sql:  "CREATE TABLE t (id int, CONSTRAINT my_pk PRIMARY KEY (id))",
			want: []string{"my_pk"},
		},
		{
			name: "inline unique",
			sql:  "CREATE TABLE t (email text UNIQUE)",
			want: []string{"t_email_key"},
		},
		{
			name: "named inline unique",
			sql:  "CREATE TABLE t (email text CONSTRAINT email_uq UNIQUE)",
			want: []string{"email_uq"},
		},
		{
			name: "multi-column table unique",
			sql:  "CREATE TABLE t (a int, b int, UNIQUE (a, b))",
			want: []string{"t_a_b_key"},
		},
		{
			name: "exclude on a plain column",
			sql:  "CREATE TABLE t (id int, EXCLUDE USING btree (id WITH =))",
			want: []string{"t_id_excl"},
		},
		{
			name: "exclude on an expression",
			sql:  "CREATE TABLE t (id int, EXCLUDE USING btree ((id * 2) WITH =))",
			want: []string{"t_expr_excl"},
		},
		{
			name: "mixed constraints in definition order",
			sql:  "CREATE TABLE t (id int PRIMARY KEY, email text UNIQUE, a int, b int, CONSTRAINT ab_uq UNIQUE (a, b))",
			want: []string{"t_pkey", "t_email_key", "ab_uq"},
		},
		{
			name: "qualified table uses the bare relation name",
			sql:  `CREATE TABLE "s"."t" (id int PRIMARY KEY)`,
			want: []string{"t_pkey"},
		},
		{
			name: "identical first choices are both returned",
			sql:  "CREATE TABLE t (a int, UNIQUE (a), CONSTRAINT t_a_key UNIQUE (a))",
			want: []string{"t_a_key", "t_a_key"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := statement.ImplicitIndexNames(tt.sql)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// The generated name is truncated to the identifier byte budget the way
// the server truncates: the longer of table and column contributions
// shrinks first, and the label always survives whole.
func TestImplicitIndexNamesTruncatesLikeTheServer(t *testing.T) {
	table := strings.Repeat("t", 70)
	got, err := statement.ImplicitIndexNames("CREATE TABLE " + table + " (id int PRIMARY KEY)")
	require.NoError(t, err)
	require.Len(t, got, 1)
	// NAMEDATALEN-1 = 63: 58 bytes of table + "_pkey".
	assert.Equal(t, strings.Repeat("t", 58)+"_pkey", got[0])
	assert.LessOrEqual(t, len(got[0]), 63)

	column := strings.Repeat("c", 70)
	got, err = statement.ImplicitIndexNames("CREATE TABLE t (" + column + " int UNIQUE)")
	require.NoError(t, err)
	require.Len(t, got, 1)
	// 63 - len("_key") - len("t_") = 57 bytes of column survive.
	assert.Equal(t, "t_"+strings.Repeat("c", 57)+"_key", got[0])
	assert.LessOrEqual(t, len(got[0]), 63)
}

func TestImplicitIndexNamesRefusesNonCreateTable(t *testing.T) {
	_, err := statement.ImplicitIndexNames("CREATE INDEX i ON t (id)")
	require.ErrorIs(t, err, statement.ErrNotCreateTable)
}

func TestImplicitIndexNamesRefusesMultipleStatements(t *testing.T) {
	_, err := statement.ImplicitIndexNames("CREATE TABLE t (id int); CREATE TABLE u (id int)")
	require.ErrorIs(t, err, statement.ErrNotOneStatement)
}
