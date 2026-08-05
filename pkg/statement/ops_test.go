package statement_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/statement"
)

func parseOneOp(t *testing.T, sql string) statement.Op {
	t.Helper()
	ops, err := statement.ParseOps(sql)
	require.NoError(t, err)
	require.Len(t, ops, 1)
	return ops[0]
}

func TestParseOpsShapes(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want statement.Op
	}{
		{
			name: "add column plain",
			sql:  "ALTER TABLE t ADD COLUMN age int",
			want: statement.Op{Kind: statement.OpAddColumn, Column: "age", NewType: "int4"},
		},
		{
			name: "add column constant default",
			sql:  "ALTER TABLE t ADD COLUMN age int DEFAULT 0",
			want: statement.Op{Kind: statement.OpAddColumn, Column: "age", NewType: "int4", Default: statement.DefaultConstant},
		},
		{
			name: "add column cast constant default",
			sql:  "ALTER TABLE t ADD COLUMN created timestamptz DEFAULT '2020-01-01'::timestamptz",
			want: statement.Op{Kind: statement.OpAddColumn, Column: "created", NewType: "timestamptz", Default: statement.DefaultConstant},
		},
		{
			name: "add column function default",
			sql:  "ALTER TABLE t ADD COLUMN id uuid DEFAULT uuid_generate_v4()",
			want: statement.Op{Kind: statement.OpAddColumn, Column: "id", NewType: "uuid", Default: statement.DefaultExpression},
		},
		{
			name: "add column value function default",
			sql:  "ALTER TABLE t ADD COLUMN created timestamptz DEFAULT CURRENT_TIMESTAMP",
			want: statement.Op{Kind: statement.OpAddColumn, Column: "created", NewType: "timestamptz", Default: statement.DefaultExpression},
		},
		{
			name: "add column serial",
			sql:  "ALTER TABLE t ADD COLUMN n serial",
			want: statement.Op{Kind: statement.OpAddColumn, Column: "n", NewType: "serial", Default: statement.DefaultExpression},
		},
		{
			name: "add column identity",
			sql:  "ALTER TABLE t ADD COLUMN n bigint GENERATED ALWAYS AS IDENTITY",
			want: statement.Op{Kind: statement.OpAddColumn, Column: "n", NewType: "int8", Default: statement.DefaultExpression},
		},
		{
			name: "add column generated stored",
			sql:  "ALTER TABLE t ADD COLUMN total numeric GENERATED ALWAYS AS (price * qty) STORED",
			want: statement.Op{Kind: statement.OpAddColumn, Column: "total", NewType: "numeric", GeneratedStored: true},
		},
		{
			name: "drop column",
			sql:  "ALTER TABLE t DROP COLUMN age",
			want: statement.Op{Kind: statement.OpDropColumn, Column: "age"},
		},
		{
			name: "alter type with mods",
			sql:  "ALTER TABLE t ALTER COLUMN name TYPE varchar(100)",
			want: statement.Op{Kind: statement.OpAlterColumnType, Column: "name", NewType: "varchar", NewTypeMods: []int32{100}},
		},
		{
			name: "alter type with using",
			sql:  "ALTER TABLE t ALTER COLUMN doc TYPE jsonb USING doc::jsonb",
			want: statement.Op{Kind: statement.OpAlterColumnType, Column: "doc", NewType: "jsonb", HasUsing: true},
		},
		{
			name: "set default",
			sql:  "ALTER TABLE t ALTER COLUMN age SET DEFAULT 1",
			want: statement.Op{Kind: statement.OpSetDefault, Column: "age"},
		},
		{
			name: "drop default",
			sql:  "ALTER TABLE t ALTER COLUMN age DROP DEFAULT",
			want: statement.Op{Kind: statement.OpDropDefault, Column: "age"},
		},
		{
			name: "set not null",
			sql:  "ALTER TABLE t ALTER COLUMN age SET NOT NULL",
			want: statement.Op{Kind: statement.OpSetNotNull, Column: "age"},
		},
		{
			name: "drop not null",
			sql:  "ALTER TABLE t ALTER COLUMN age DROP NOT NULL",
			want: statement.Op{Kind: statement.OpDropNotNull, Column: "age"},
		},
		{
			name: "set statistics",
			sql:  "ALTER TABLE t ALTER COLUMN age SET STATISTICS 500",
			want: statement.Op{Kind: statement.OpSetColumnOptions, Column: "age"},
		},
		{
			name: "rename column",
			sql:  "ALTER TABLE t RENAME COLUMN a TO b",
			want: statement.Op{Kind: statement.OpRenameColumn, Column: "a", Name: "b"},
		},
		{
			name: "rename table",
			sql:  "ALTER TABLE t RENAME TO t2",
			want: statement.Op{Kind: statement.OpRenameTable, Name: "t2"},
		},
		{
			name: "rename index",
			sql:  "ALTER INDEX i RENAME TO i2",
			want: statement.Op{Kind: statement.OpRenameIndex, Name: "i2"},
		},
		{
			name: "set schema",
			sql:  "ALTER TABLE t SET SCHEMA s2",
			want: statement.Op{Kind: statement.OpSetSchema, Name: "s2"},
		},
		{
			name: "set tablespace",
			sql:  "ALTER TABLE t SET TABLESPACE fast",
			want: statement.Op{Kind: statement.OpSetTablespace, Name: "fast"},
		},
		{
			name: "set rel options",
			sql:  "ALTER TABLE t SET (fillfactor = 70)",
			want: statement.Op{Kind: statement.OpSetRelOptions},
		},
		{
			name: "add primary key",
			sql:  "ALTER TABLE t ADD CONSTRAINT t_pkey PRIMARY KEY (id)",
			want: statement.Op{Kind: statement.OpAddConstraint, Name: "t_pkey", Constraint: statement.ConstraintPrimaryKey, Columns: []string{"id"}},
		},
		{
			name: "add unique using index",
			sql:  "ALTER TABLE t ADD CONSTRAINT u UNIQUE USING INDEX u_idx",
			want: statement.Op{Kind: statement.OpAddConstraint, Name: "u", Constraint: statement.ConstraintUnique, UsingIndex: true},
		},
		{
			name: "add check not valid",
			sql:  "ALTER TABLE t ADD CONSTRAINT c CHECK (age > 0) NOT VALID",
			want: statement.Op{Kind: statement.OpAddConstraint, Name: "c", Constraint: statement.ConstraintCheck, NotValid: true},
		},
		{
			name: "add foreign key",
			sql:  "ALTER TABLE t ADD CONSTRAINT fk FOREIGN KEY (pid) REFERENCES p (id)",
			want: statement.Op{Kind: statement.OpAddConstraint, Name: "fk", Constraint: statement.ConstraintForeignKey},
		},
		{
			name: "validate constraint",
			sql:  "ALTER TABLE t VALIDATE CONSTRAINT c",
			want: statement.Op{Kind: statement.OpValidateConstraint, Name: "c"},
		},
		{
			name: "drop constraint",
			sql:  "ALTER TABLE t DROP CONSTRAINT c",
			want: statement.Op{Kind: statement.OpDropConstraint, Name: "c"},
		},
		{
			name: "attach partition",
			sql:  "ALTER TABLE t ATTACH PARTITION p FOR VALUES FROM (1) TO (10)",
			want: statement.Op{Kind: statement.OpAttachPartition},
		},
		{
			name: "detach partition",
			sql:  "ALTER TABLE t DETACH PARTITION p",
			want: statement.Op{Kind: statement.OpDetachPartition},
		},
		{
			name: "detach partition concurrently",
			sql:  "ALTER TABLE t DETACH PARTITION p CONCURRENTLY",
			want: statement.Op{Kind: statement.OpDetachPartition, Concurrent: true},
		},
		{
			name: "create index",
			sql:  "CREATE INDEX i ON t (a)",
			want: statement.Op{Kind: statement.OpCreateIndex, Name: "i"},
		},
		{
			name: "create unique index concurrently",
			sql:  "CREATE UNIQUE INDEX CONCURRENTLY i ON t (a)",
			want: statement.Op{Kind: statement.OpCreateIndex, Name: "i", Concurrent: true, Unique: true},
		},
		{
			name: "drop index",
			sql:  "DROP INDEX i",
			want: statement.Op{Kind: statement.OpDropIndex},
		},
		{
			name: "drop index concurrently",
			sql:  "DROP INDEX CONCURRENTLY i",
			want: statement.Op{Kind: statement.OpDropIndex, Concurrent: true},
		},
		{
			name: "reindex",
			sql:  "REINDEX INDEX i",
			want: statement.Op{Kind: statement.OpReindex, Name: "i"},
		},
		{
			name: "reindex concurrently",
			sql:  "REINDEX INDEX CONCURRENTLY i",
			want: statement.Op{Kind: statement.OpReindex, Name: "i", Concurrent: true},
		},
		{
			name: "create table",
			sql:  "CREATE TABLE t (id int PRIMARY KEY)",
			want: statement.Op{Kind: statement.OpCreateTable},
		},
		{
			name: "unrecognized statement",
			sql:  "VACUUM FULL t",
			want: statement.Op{Kind: statement.OpUnrecognized},
		},
		{
			name: "unrecognized subcommand",
			sql:  "ALTER TABLE t ENABLE ROW LEVEL SECURITY",
			want: statement.Op{Kind: statement.OpUnrecognized},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, parseOneOp(t, tc.sql))
		})
	}
}

func TestParseOpsMultipleSubcommands(t *testing.T) {
	ops, err := statement.ParseOps("ALTER TABLE t ADD COLUMN a int, DROP COLUMN b")
	require.NoError(t, err)
	require.Len(t, ops, 2)
	assert.Equal(t, statement.OpAddColumn, ops[0].Kind)
	assert.Equal(t, statement.OpDropColumn, ops[1].Kind)
}

func TestParseOpsRejectsMultipleStatements(t *testing.T) {
	_, err := statement.ParseOps("SELECT 1; SELECT 2")
	assert.ErrorIs(t, err, statement.ErrNotOneStatement)
}

func TestConcurrentlyRewrites(t *testing.T) {
	// The rewrite contract is syntactic: the result must parse back with
	// the concurrency flag set. Exact deparser wording is not a contract.
	for _, sql := range []string{
		"CREATE INDEX i ON t (a)",
		"DROP INDEX i",
		"REINDEX INDEX i",
		"ALTER TABLE t DETACH PARTITION p",
	} {
		t.Run(sql, func(t *testing.T) {
			safer, err := statement.Concurrently(sql)
			require.NoError(t, err)
			op := parseOneOp(t, safer)
			assert.True(t, op.Concurrent, "rewritten statement must be concurrent: %s", safer)
		})
	}
}

func TestConcurrentlyRefusesOtherStatements(t *testing.T) {
	_, err := statement.Concurrently("ALTER TABLE t ADD COLUMN a int")
	assert.ErrorIs(t, err, statement.ErrNotRewritable)
}

func TestAddNotValidRewritesNamedCheck(t *testing.T) {
	safer, name, err := statement.AddNotValid("ALTER TABLE t ADD CONSTRAINT c CHECK (age > 0)")
	require.NoError(t, err)
	assert.Equal(t, "c", name)
	op := parseOneOp(t, safer)
	assert.True(t, op.NotValid, "rewritten constraint must be NOT VALID: %s", safer)
}

func TestAddNotValidRefusals(t *testing.T) {
	cases := []struct {
		name string
		sql  string
	}{
		{"unnamed constraint", "ALTER TABLE t ADD CHECK (age > 0)"},
		{"primary key", "ALTER TABLE t ADD CONSTRAINT p PRIMARY KEY (id)"},
		{"not an add constraint", "ALTER TABLE t DROP COLUMN a"},
		{"multiple subcommands", "ALTER TABLE t ADD CONSTRAINT c CHECK (a > 0), DROP COLUMN b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := statement.AddNotValid(tc.sql)
			assert.ErrorIs(t, err, statement.ErrNotValidNotApplicable)
		})
	}
}
