package migrate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// The acknowledgement applies to exactly the gate refusals the registry
// marks eligible: one plain DROP INDEX, REINDEX INDEX, or REINDEX TABLE.
// The already-concurrent forms, the multi-relation forms, and every other
// gated kind stand refused whether or not the flag is present, and an
// absent acknowledgement accepts nothing.
func TestAcceptedRefusal(t *testing.T) {
	gate := func(t *testing.T, sql string) verdict.Verdict {
		t.Helper()
		st, err := statement.ParseOne(sql)
		require.NoError(t, err)
		v, refused := Gate(st)
		require.True(t, refused, "the statement must be gate-refused for the acknowledgement to have anything to accept")
		return v
	}

	for name, sql := range map[string]string{
		"drop index":    "DROP INDEX app.orders_created_idx",
		"reindex index": "REINDEX INDEX app.orders_created_idx",
		"reindex table": "REINDEX TABLE app.orders",
	} {
		t.Run("accepts "+name, func(t *testing.T) {
			proof, ok := AcceptedRefusal("app.orders", gate(t, sql))
			require.True(t, ok)
			assert.Equal(t, verdict.ReasonIndexStatement, proof.Reason())
			assert.Equal(t, verdict.ClassByDesign, proof.Class())
		})
	}

	for name, sql := range map[string]string{
		"drop index concurrently":    "DROP INDEX CONCURRENTLY app.orders_created_idx",
		"reindex table concurrently": "REINDEX TABLE CONCURRENTLY app.orders",
		"drop of several indexes":    "DROP INDEX app.a, app.b",
		"reindex schema":             "REINDEX SCHEMA app",
		"create table":               "CREATE TABLE app.orders (id int)",
		"drop table":                 "DROP TABLE app.orders",
	} {
		t.Run("does not accept "+name, func(t *testing.T) {
			_, ok := AcceptedRefusal("app.orders", gate(t, sql))
			assert.False(t, ok)
		})
	}

	t.Run("an absent acknowledgement accepts nothing", func(t *testing.T) {
		_, ok := AcceptedRefusal("", gate(t, "DROP INDEX app.orders_created_idx"))
		assert.False(t, ok)
	})

	t.Run("a verdict that carries no refusal accepts nothing", func(t *testing.T) {
		_, ok := AcceptedRefusal("app.orders", verdict.Verdict{Outcome: verdict.OutcomeExecuted})
		assert.False(t, ok)
	})
}

// The two acknowledgements name different decisions; both at once is
// rejected at the front door before any database work.
func TestRunRejectsForceWithAcceptBlocking(t *testing.T) {
	st, err := statement.ParseOne("DROP INDEX app.orders_created_idx")
	require.NoError(t, err)
	opts := DefaultOptions()
	opts.Force = "app.orders"
	opts.AcceptBlocking = "app.orders"

	v, err := Run(t.Context(), nil, st, opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AcceptBlocking")
	assert.Equal(t, verdict.Verdict{}, v)
}

// The catalog lookup receives the relation exactly as the grammar spelled
// it: a mixed-case or reserved-word name is quoted so to_regclass resolves
// the relation the statement will act on, and an unqualified name is left
// to the session search_path.
func TestRegclassName(t *testing.T) {
	assert.Equal(t, `"Orders_Idx"`,
		regclassName(statement.IndexRelation{Name: "Orders_Idx", Kind: statement.IndexRelationIndex}))
	assert.Equal(t, `"app"."order"`,
		regclassName(statement.IndexRelation{Schema: "app", Name: "order", Kind: statement.IndexRelationTable}))
}

// The acknowledgement resolves a relation of the kind the statement
// operates on and nothing else: DROP INDEX / REINDEX INDEX take a plain or
// partitioned index; REINDEX TABLE takes what PostgreSQL's REINDEX TABLE
// takes — a table, a partitioned table, or a materialized view. A name
// that resolves to any other kind is the wrong-kind error, not "not found".
func TestRelationKindMatches(t *testing.T) {
	for _, tc := range []struct {
		kind    statement.IndexRelationKind
		relkind string
		want    bool
	}{
		{statement.IndexRelationIndex, "i", true},
		{statement.IndexRelationIndex, "I", true},
		{statement.IndexRelationIndex, "r", false},
		{statement.IndexRelationIndex, "p", false},
		{statement.IndexRelationIndex, "m", false},
		{statement.IndexRelationTable, "r", true},
		{statement.IndexRelationTable, "p", true},
		{statement.IndexRelationTable, "m", true},
		{statement.IndexRelationTable, "i", false},
		{statement.IndexRelationTable, "v", false},
		{statement.IndexRelationTable, "S", false},
		{statement.IndexRelationNone, "r", false},
	} {
		assert.Equal(t, tc.want, relationKindMatches(tc.kind, tc.relkind), "kind %d relkind %q", tc.kind, tc.relkind)
	}
}
