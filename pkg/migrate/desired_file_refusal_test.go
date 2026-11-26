package migrate

import (
	"errors"
	"fmt"
	"testing"

	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A desired file refused before anything is planned gets a refusal verdict
// naming the refused statement, classed as the registry classes its cause:
// a domain file goes to the operator who runs catalog work, a file of
// partitions waits on a capability, and a second unrelated table, a table in
// an enum type file, or a label listed twice is refused by design.
func TestDesiredFileRefusalVerdicts(t *testing.T) {
	tests := []struct {
		name   string
		sql    string
		stmt   string
		class  verdict.Class
		owner  verdict.Owner
		detail string
	}{
		{"domain file", "CREATE DOMAIN email AS text",
			"CREATE DOMAIN email AS text", verdict.ClassNoOnlineSafetyProblem, verdict.OwnerDirectOperator,
			"a desired file holds one CREATE TABLE, its indexes, and its row security, or one enum type, so this statement is not allowed in it; nothing was planned or executed"},
		{"enum type in a table file", "CREATE TYPE mood AS ENUM ('happy', 'sad'); CREATE TABLE t (id int)",
			"CREATE TYPE mood AS ENUM ('happy', 'sad')", verdict.ClassNoOnlineSafetyProblem, verdict.OwnerDirectOperator,
			"a desired file holds one CREATE TABLE, its indexes, and its row security, or one enum type, so this statement is not allowed in it; nothing was planned or executed"},
		{"enum label listed twice", "CREATE TYPE mood AS ENUM ('happy', 'happy')",
			"unused", verdict.ClassByDesign, "",
			"statement 1: a desired enum type's labels must be distinct, non-empty, and at most 63 bytes: \"happy\" is listed twice; nothing was planned or executed"},
		{"data change", "CREATE TABLE t (id int); INSERT INTO t VALUES (1)",
			"INSERT INTO t VALUES (1)", verdict.ClassNoOnlineSafetyProblem, verdict.OwnerDataChangeRunner,
			"a desired file holds one CREATE TABLE, its indexes, and its row security, or one enum type, so this statement is not allowed in it; nothing was planned or executed"},
		{"change statement", "CREATE TABLE t (id int); ALTER TABLE t ADD COLUMN c int",
			"ALTER TABLE t ADD COLUMN c int", verdict.ClassByDesign, "",
			"a desired file holds one CREATE TABLE, its indexes, and its row security, or one enum type, so this statement is not allowed in it; nothing was planned or executed"},
		{"partitions file",
			"CREATE TABLE log_p1 PARTITION OF log FOR VALUES FROM (1) TO (2); " +
				"CREATE TABLE log_p2 PARTITION OF log FOR VALUES FROM (2) TO (3)",
			"CREATE TABLE log_p2 PARTITION OF log FOR VALUES FROM (2) TO (3)", verdict.ClassCapabilityBoundary, "",
			"a desired file holds exactly one CREATE TABLE, and the declarative model does not plan partitions; nothing was planned or executed"},
		{"renaming the empty enum label", "CREATE TABLE t (id int); ALTER TYPE mood RENAME VALUE '' TO 'blue'",
			"ALTER TYPE mood RENAME VALUE '' TO 'blue'", verdict.ClassCapabilityBoundary, "",
			"a desired file holds one CREATE TABLE, its indexes, and its row security, or one enum type, so this statement is not allowed in it; nothing was planned or executed"},
		{"two unrelated tables", "CREATE TABLE a (id int); CREATE TABLE b (id int)",
			"CREATE TABLE b (id int)", verdict.ClassByDesign, "",
			"a desired file holds exactly one CREATE TABLE; nothing was planned or executed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := statement.ParseDesiredFile(tt.sql)
			require.Error(t, err)
			v, ok := DesiredFileRefusal(err, "unused")
			require.True(t, ok, "classified: %v", err)
			assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
			assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)
			assert.Equal(t, tt.class, v.Class)
			assert.Equal(t, tt.owner, v.Owner)
			assert.Equal(t, tt.stmt, v.Statement)
			assert.Equal(t, tt.detail, v.Detail)
		})
	}
}

// A desired file that refuses several statements still gets one verdict, and
// its class, statement, and detail all describe the same refusal: a partition
// anywhere in the file outranks a statement the file may not hold.
func TestDesiredFileRefusalOfSeveralStatementsDescribesOne(t *testing.T) {
	_, err := statement.ParseDesiredFile("CREATE TABLE log (id int, at date) PARTITION BY RANGE (at);\n" +
		"COMMENT ON TABLE log IS 'events';\n" +
		"CREATE TABLE log_p1 PARTITION OF log FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');\n")
	require.Error(t, err)
	refused, ok := errors.AsType[*statement.RefusedStatementsError](err)
	require.True(t, ok, "%v", err)
	require.Len(t, refused.Refusals, 2, "the comment and the partition are both reported")

	v, ok := DesiredFileRefusal(err, "unused")
	require.True(t, ok)
	assert.Equal(t, verdict.ClassCapabilityBoundary, v.Class)
	assert.Equal(t, "CREATE TABLE log_p1 PARTITION OF log FOR VALUES FROM ('2026-01-01') TO ('2026-02-01')", v.Statement)
	assert.Equal(t, "a desired file holds exactly one CREATE TABLE, and the declarative model does not plan partitions; nothing was planned or executed", v.Detail)
}

// When a second table precedes a partition, the verdict still describes the
// partition: its class decides the file's verdict, so its statement and
// detail do too, not the earlier table's.
func TestDesiredFileRefusalDescribesThePartitionAfterASecondTable(t *testing.T) {
	_, err := statement.ParseDesiredFile("CREATE TABLE a (id int);\n" +
		"CREATE TABLE b (id int);\n" +
		"CREATE TABLE p PARTITION OF a DEFAULT;\n")
	require.Error(t, err)
	refused, ok := errors.AsType[*statement.RefusedStatementsError](err)
	require.True(t, ok, "%v", err)
	require.Len(t, refused.Refusals, 2, "the second table and the partition are both reported")

	v, ok := DesiredFileRefusal(err, "unused")
	require.True(t, ok)
	assert.Equal(t, verdict.ClassCapabilityBoundary, v.Class)
	assert.Equal(t, "CREATE TABLE p PARTITION OF a DEFAULT", v.Statement)
	assert.Equal(t, "a desired file holds exactly one CREATE TABLE, and the declarative model does not plan partitions; nothing was planned or executed", v.Detail)
}

// A planner refusal of the desired table carries no statement of its own, so
// the verdict names the statement the caller passes.
func TestDesiredFileRefusalNamesTheCallersStatementForAPlannerRefusal(t *testing.T) {
	err := fmt.Errorf("desired table %q: %w", "log_p1", schemadiff.ErrDesiredPartitionOf)
	v, ok := DesiredFileRefusal(err, "CREATE TABLE log_p1 PARTITION OF log DEFAULT")
	require.True(t, ok)
	assert.Equal(t, verdict.ClassCapabilityBoundary, v.Class)
	assert.Equal(t, "CREATE TABLE log_p1 PARTITION OF log DEFAULT", v.Statement)
	assert.Equal(t, err.Error()+"; nothing was planned or executed", v.Detail)
}

// A syntax error, or an admission rule the registry does not classify, is an
// input error, not a refusal.
func TestDesiredFileRefusalLeavesInputErrorsAlone(t *testing.T) {
	for _, sql := range []string{"CREATE TABEL t (id int)", "CREATE TABLE prod.t (id int)", ""} {
		_, err := statement.ParseDesiredFile(sql)
		require.Error(t, err)
		_, ok := DesiredFileRefusal(err, "")
		assert.False(t, ok, "%q is not a refusal: %v", sql, err)
	}
}

// A change the diff does not derive is refused as a desired-file refusal,
// classed by the cause the diff names: a storage parameter or identity
// sequence difference is the owner's to change directly, a persistence
// change needs a rewrite, and a partitioning difference waits on a
// capability. The detail keeps the diff's message.
func TestDesiredFileRefusalClassesUnsupportedChanges(t *testing.T) {
	model := func(edit func(*schemadiff.Model)) schemadiff.Model {
		m := schemadiff.Model{Table: "events", Columns: []schemadiff.Column{{Name: "id", Type: "bigint", NotNull: true}}}
		edit(&m)
		return m
	}
	identity := func(maxValue int64) func(*schemadiff.Model) {
		return func(m *schemadiff.Model) {
			m.Columns[0].Identity = schemadiff.IdentityAlways
			m.Columns[0].Sequence = &schemadiff.SequenceOptions{
				Type: "bigint", Start: 1, Increment: 1, Min: 1, Max: maxValue, Cache: 1,
			}
		}
	}
	unchanged := func(*schemadiff.Model) {}
	tests := []struct {
		name    string
		live    schemadiff.Model
		desired schemadiff.Model
		cause   schemadiff.UnsupportedCause
		class   verdict.Class
		reason  verdict.Reason
		owner   verdict.Owner
	}{
		{"storage parameters", model(unchanged), model(func(m *schemadiff.Model) { m.Options = []string{"fillfactor=70"} }),
			schemadiff.CauseStorageParameters, verdict.ClassNoOnlineSafetyProblem, verdict.ReasonUnsupportedStatement, verdict.OwnerDirectOperator},
		{"identity sequence", model(identity(9223372036854775807)), model(identity(5000)),
			schemadiff.CauseIdentitySequence, verdict.ClassNoOnlineSafetyProblem, verdict.ReasonUnsupportedStatement, verdict.OwnerDirectOperator},
		{"persistence", model(unchanged), model(func(m *schemadiff.Model) { m.Unlogged = true }),
			schemadiff.CausePersistence, verdict.ClassCapabilityBoundary, verdict.ReasonRewriteRequired, ""},
		{"partition key", model(unchanged), model(func(m *schemadiff.Model) { m.PartitionKey = "RANGE (id)" }),
			schemadiff.CausePartitionKey, verdict.ClassCapabilityBoundary, verdict.ReasonUnsupportedStatement, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := schemadiff.Diff("public", tt.live, tt.desired)
			require.ErrorIs(t, err, schemadiff.ErrUnsupportedChange)
			refused, isRefusal := errors.AsType[*schemadiff.UnsupportedChangeError](err)
			require.True(t, isRefusal, "the diff names its cause: %v", err)
			assert.Equal(t, tt.cause, refused.Cause)
			v, ok := DesiredFileRefusal(fmt.Errorf("plan: %w", err), "CREATE TABLE events (id bigint NOT NULL)")
			require.True(t, ok, "classified: %v", err)
			assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
			assert.Equal(t, tt.reason, v.Reason)
			assert.Equal(t, tt.class, v.Class)
			assert.Equal(t, tt.owner, v.Owner)
			assert.Equal(t, "CREATE TABLE events (id bigint NOT NULL)", v.Statement)
			assert.Equal(t, fmt.Sprintf("plan: %v; nothing was planned or executed", err), v.Detail)
		})
	}
}

// A live enum type the file cannot converge is refused as a desired-file
// refusal, classed by its cause: a missing type or a dropped label is the
// owner's to fix directly (CREATE TYPE, ALTER TYPE ... RENAME VALUE), labels
// listed out of live order are the file's to correct, and a name that holds
// another kind of type waits on a capability.
func TestDesiredFileRefusalClassesEnumTypeChanges(t *testing.T) {
	file, err := statement.ParseDesiredFile("CREATE TYPE mood AS ENUM ('happy', 'sad')")
	require.NoError(t, err)
	desired, ok := file.EnumType()
	require.True(t, ok)
	tests := []struct {
		name  string
		err   error
		class verdict.Class
		owner verdict.Owner
	}{
		{"missing type", &schemadiff.UnsupportedChangeError{Cause: schemadiff.CauseEnumTypeMissing},
			verdict.ClassNoOnlineSafetyProblem, verdict.OwnerDirectOperator},
		{"not an enum", &schemadiff.UnsupportedChangeError{Cause: schemadiff.CauseEnumTypeKind},
			verdict.ClassCapabilityBoundary, ""},
	}
	for _, live := range []struct {
		name   string
		labels []string
		class  verdict.Class
		owner  verdict.Owner
	}{
		{"label removed", []string{"happy", "sad", "meh"}, verdict.ClassNoOnlineSafetyProblem, verdict.OwnerDirectOperator},
		{"labels reordered", []string{"sad", "happy"}, verdict.ClassByDesign, ""},
	} {
		_, diffErr := schemadiff.DiffEnumType(schemadiff.EnumType{Schema: "public", Name: "mood", Labels: live.labels}, desired)
		require.ErrorIs(t, diffErr, schemadiff.ErrUnsupportedChange)
		tests = append(tests, struct {
			name  string
			err   error
			class verdict.Class
			owner verdict.Owner
		}{live.name, diffErr, live.class, live.owner})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sql := desired.Statements()[0].SQL()
			v, ok := DesiredFileRefusal(fmt.Errorf("plan: %w", tt.err), sql)
			require.True(t, ok, "classified: %v", tt.err)
			assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
			assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)
			assert.Equal(t, tt.class, v.Class)
			assert.Equal(t, tt.owner, v.Owner)
			assert.Equal(t, sql, v.Statement)
			assert.Equal(t, fmt.Sprintf("plan: %v; nothing was planned or executed", tt.err), v.Detail)
		})
	}
}
