package cli

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/statement"
)

// documentsRowSecurity is a desired file that declares row security: diff and
// migrate --desired admit it through ParseDesiredFile, so fmt must too.
const documentsRowSecurity = `create table documents (
  id bigint primary key,
  team_id bigint not null,
  title text not null
);
create index documents_team_id_idx on documents (team_id);
alter table documents enable row level security;
create policy documents_team_read on documents
  for select
  to public
  using (team_id in (select team_id from public.team_members where member = current_user));
comment on policy documents_team_read on documents is 'members read their team''s documents';
alter table documents force row level security;`

// fmt prints every statement of a row-security file in the deparser's
// canonical form, in the order ParseDesiredFile admits them: the table and its
// indexes, the row-security settings and policies, then policy comments. The
// COMMENT ON POLICY therefore moves after FORCE ROW LEVEL SECURITY.
func TestFmtCanonicalizesRowSecurityFile(t *testing.T) {
	cmd := &FmtCmd{}
	var out strings.Builder
	require.NoError(t, cmd.runFmt(strings.NewReader(documentsRowSecurity), &out))
	assert.Equal(t,
		"CREATE TABLE documents (id bigint PRIMARY KEY, team_id bigint NOT NULL, title text NOT NULL);\n"+
			"CREATE INDEX documents_team_id_idx ON documents USING btree (team_id);\n"+
			"ALTER TABLE documents ENABLE ROW LEVEL SECURITY;\n"+
			"CREATE POLICY documents_team_read ON documents FOR SELECT TO public USING (team_id IN (SELECT team_id FROM public.team_members WHERE member = current_user));\n"+
			"ALTER TABLE documents FORCE ROW LEVEL SECURITY;\n"+
			"COMMENT ON POLICY documents_team_read ON documents IS 'members read their team''s documents';\n",
		out.String())
}

// fmt output of a row-security file is admitted again as the same desired
// file, and formatting it a second time changes nothing.
func TestFmtRowSecurityFileRoundTrips(t *testing.T) {
	cmd := &FmtCmd{}
	var first strings.Builder
	require.NoError(t, cmd.runFmt(strings.NewReader(documentsRowSecurity), &first))

	original, err := statement.ParseDesiredFile(documentsRowSecurity)
	require.NoError(t, err)
	reparsed, err := statement.ParseDesiredFile(first.String())
	require.NoError(t, err)
	_, ok := reparsed.RowSecurity()
	assert.True(t, ok, "formatted output still declares row security")
	assert.Equal(t, original, reparsed)

	var second strings.Builder
	require.NoError(t, cmd.runFmt(strings.NewReader(first.String()), &second))
	assert.Equal(t, first.String(), second.String())
}

// A plain file formats exactly as it does through ParseDesired: routing fmt
// through ParseDesiredFile changes nothing for a file without row security.
func TestFmtPlainFileMatchesParseDesired(t *testing.T) {
	const plain = `create table events (
  id bigint primary key,
  name varchar(50) not null
);
create index events_name_idx on events (name);`
	cmd := &FmtCmd{}
	var out strings.Builder
	require.NoError(t, cmd.runFmt(strings.NewReader(plain), &out))

	ds, err := statement.ParseDesired(plain)
	require.NoError(t, err)
	var want strings.Builder
	for _, st := range ds.Statements() {
		want.WriteString(st.SQL() + ";\n")
	}
	assert.Equal(t, want.String(), out.String())
	assert.Equal(t,
		"CREATE TABLE events (id bigint PRIMARY KEY, name varchar(50) NOT NULL);\n"+
			"CREATE INDEX events_name_idx ON events USING btree (name);\n",
		out.String())
}

// A row-security file with a comment is refused like any other commented
// file: the deparser would drop it.
func TestFmtRefusesCommentedRowSecurityFile(t *testing.T) {
	cmd := &FmtCmd{}
	var out strings.Builder
	err := cmd.runFmt(strings.NewReader(`CREATE TABLE documents (
  id bigint PRIMARY KEY
);
-- only owners may read
ALTER TABLE documents ENABLE ROW LEVEL SECURITY;`), &out)
	require.ErrorIs(t, err, statement.ErrCommentLoss)
	assert.Empty(t, out.String())
}

// fmt keeps the order policies and settings were written in and moves only
// policy comments. It carries each policy's mode, command, roles and
// expressions through the deparser unchanged, drops the default AS
// PERMISSIVE and FOR ALL, and spells an omitted role list TO public.
func TestFmtRowSecurityKeepsPolicyOrderAndForms(t *testing.T) {
	const in = `create table documents (id bigint primary key, owner text not null);
create policy z_writes on documents as restrictive for update to app_rw, "App Admin"
  using (owner = current_user) with check (owner <> 'root');
comment on policy z_writes on documents is 'writers only';
alter table documents enable row level security;
create policy m_all on documents as permissive for all to public using (true);
alter table documents no force row level security;
create policy a_reads on documents for select using (true);
comment on policy a_reads on documents is 'everyone reads';`
	var out strings.Builder
	require.NoError(t, (&FmtCmd{}).runFmt(strings.NewReader(in), &out))
	assert.Equal(t,
		"CREATE TABLE documents (id bigint PRIMARY KEY, owner text NOT NULL);\n"+
			"CREATE POLICY z_writes ON documents AS RESTRICTIVE FOR UPDATE TO app_rw, \"App Admin\" USING (owner = current_user) WITH CHECK (owner <> 'root');\n"+
			"ALTER TABLE documents ENABLE ROW LEVEL SECURITY;\n"+
			"CREATE POLICY m_all ON documents TO public USING (true);\n"+
			"ALTER TABLE documents NO FORCE ROW LEVEL SECURITY;\n"+
			"CREATE POLICY a_reads ON documents FOR SELECT TO public USING (true);\n"+
			"COMMENT ON POLICY z_writes ON documents IS 'writers only';\n"+
			"COMMENT ON POLICY a_reads ON documents IS 'everyone reads';\n",
		out.String())
}

// A row-security file the admission rules refuse is refused by fmt with the
// same typed error diff gets, and nothing is written.
func TestFmtRefusesRowSecurityFileTheFrontDoorRefuses(t *testing.T) {
	for name, tc := range map[string]struct {
		sql  string
		want error
	}{
		"policy without ENABLE or DISABLE": {
			sql: `CREATE TABLE t (id bigint PRIMARY KEY);
CREATE POLICY p ON t USING (true);`,
			want: statement.ErrRowSecurityDeclaration,
		},
		"policy reads an unqualified table": {
			sql: `CREATE TABLE t (id bigint PRIMARY KEY);
ALTER TABLE t ENABLE ROW LEVEL SECURITY;
CREATE POLICY p ON t USING (EXISTS (SELECT 1 FROM members));`,
			want: statement.ErrPolicyRelationDependency,
		},
		"foreign key beside row security": {
			sql: `CREATE TABLE t (id bigint PRIMARY KEY, o bigint REFERENCES o (id));
ALTER TABLE t ENABLE ROW LEVEL SECURITY;`,
			want: statement.ErrRowSecurityForeignKey,
		},
		"unparseable policy": {
			sql: `CREATE TABLE t (id bigint PRIMARY KEY);
ALTER TABLE t ENABLE ROW LEVEL SECURITY;
CREATE POLICY p ON t USING (true`,
			want: statement.ErrParseDesired,
		},
	} {
		t.Run(name, func(t *testing.T) {
			var out strings.Builder
			err := (&FmtCmd{}).runFmt(strings.NewReader(tc.sql), &out)
			require.ErrorIs(t, err, tc.want)
			assert.Empty(t, out.String())
		})
	}
}
