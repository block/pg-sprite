package testutil

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reports records what AssertCatalogQualified reports instead of failing.
type reports struct {
	got []string
}

func (r *reports) Helper() {}

func (r *reports) Errorf(format string, args ...any) {
	r.got = append(r.got, fmt.Sprintf(format, args...))
}

// A subquery in FROM or JOIN, LATERAL or not, names no relation of its own,
// so a fully qualified read with one passes.
func TestCatalogQualifiedPassesASubqueryInFromOrJoin(t *testing.T) {
	r := &reports{}
	AssertCatalogQualified(r, `
	SELECT pos.write_lsn, extra.rendered
	FROM pg_catalog.pg_replication_slots s
	CROSS JOIN (
	       SELECT pg_catalog.pg_current_wal_lsn() AS write_lsn
	       ) AS pos
	CROSS JOIN LATERAL (SELECT pg_catalog.to_jsonb(s) AS rendered) AS extra
	WHERE s.slot_name OPERATOR(pg_catalog.=) $1`)
	assert.Empty(t, r.got)
}

// A parenthesised join is not a subquery: the relation it opens with is a
// relation, reported when bare and passed when qualified.
func TestCatalogQualifiedReportsABareRelationInAParenthesisedJoin(t *testing.T) {
	bare := &reports{}
	AssertCatalogQualified(bare, `
	SELECT 1
	FROM (pg_replication_slots s CROSS JOIN pg_catalog.pg_class c)
	WHERE s.slot_name OPERATOR(pg_catalog.=) $1`)
	require.Len(t, bare.got, 1)
	assert.Contains(t, bare.got[0], `relation "pg_replication_slots" is not qualified`)

	qualified := &reports{}
	AssertCatalogQualified(qualified, `
	SELECT 1
	FROM (pg_catalog.pg_replication_slots s CROSS JOIN pg_catalog.pg_class c)
	WHERE s.slot_name OPERATOR(pg_catalog.=) $1`)
	assert.Empty(t, qualified.got)
}

// assertReportedOnce runs AssertCatalogQualified on query and asserts it
// reports exactly one finding, which contains want.
func assertReportedOnce(t *testing.T, query, want string) {
	t.Helper()
	r := &reports{}
	AssertCatalogQualified(r, query)
	if assert.Len(t, r.got, 1, "reports: %v", r.got) {
		assert.Contains(t, r.got[0], want)
	}
}

// assertPasses runs AssertCatalogQualified on query with own excepted and
// asserts it reports nothing.
func assertPasses(t *testing.T, query string, own ...string) {
	t.Helper()
	r := &reports{}
	AssertCatalogQualified(r, query, own...)
	assert.Empty(t, r.got)
}

// A space between the cast marker and the type name still names text
// through the search_path.
func TestCatalogQualifiedReportsACastSpacedFromItsMarker(t *testing.T) {
	assertReportedOnce(t, `
	SELECT s.slot_name:: text
	FROM pg_catalog.pg_replication_slots s`,
		`type "text" is not qualified`)
	assertPasses(t, `
	SELECT s.slot_name:: pg_catalog.text
	FROM pg_catalog.pg_replication_slots s`)
}

// A quoted type name is still resolved through the search_path.
func TestCatalogQualifiedReportsACastToAQuotedType(t *testing.T) {
	assertReportedOnce(t, `
	SELECT s.slot_name::"text"
	FROM pg_catalog.pg_replication_slots s`,
		`type "text" is not qualified`)
	assertPasses(t, `
	SELECT s.slot_name::pg_catalog."text"
	FROM pg_catalog.pg_replication_slots s`)
}

// A space between a function's name and its parenthesis still calls the
// function the search_path finds first.
func TestCatalogQualifiedReportsACallSpacedFromItsParenthesis(t *testing.T) {
	assertReportedOnce(t, `
	SELECT to_jsonb (s)
	FROM pg_catalog.pg_replication_slots s`,
		`function "to_jsonb" is not qualified`)
	assertPasses(t, `
	SELECT pg_catalog.to_jsonb (s)
	FROM pg_catalog.pg_replication_slots s`)
}

// A type's constructor form is a call of a function of the type's name.
func TestCatalogQualifiedReportsATypeConstructor(t *testing.T) {
	assertReportedOnce(t, `
	SELECT timestamptz (pg_catalog.now())`,
		`function "timestamptz" is not qualified`)
	assertPasses(t, `
	SELECT pg_catalog.timestamptz (pg_catalog.now())`)
}

// IN over a list compares each element with the = the search_path finds;
// = ANY spelled with OPERATOR(pg_catalog.=) is its qualified form.
func TestCatalogQualifiedReportsInOverAList(t *testing.T) {
	assertReportedOnce(t, `
	SELECT s.slot_name
	FROM pg_catalog.pg_replication_slots s
	WHERE s.slot_type IN ('logical', 'physical')`,
		`IN (list) compares through operator "=", which the search_path resolves`)
	assertPasses(t, `
	SELECT s.slot_name
	FROM pg_catalog.pg_replication_slots s
	WHERE s.slot_type OPERATOR(pg_catalog.=) ANY (ARRAY['logical', 'physical']::pg_catalog.text[])`)
}

// IN over a subquery compares with the = the search_path finds, however
// qualified the subquery itself is.
func TestCatalogQualifiedReportsInOverASubquery(t *testing.T) {
	assertReportedOnce(t, `
	SELECT s.slot_name
	FROM pg_catalog.pg_replication_slots s
	WHERE s.database IN (SELECT d.datname FROM pg_catalog.pg_database d)`,
		`IN (subquery) compares through an unqualified =; write x OPERATOR(pg_catalog.=) ANY (subquery)`)
	assertPasses(t, `
	SELECT s.slot_name
	FROM pg_catalog.pg_replication_slots s
	WHERE s.database OPERATOR(pg_catalog.=) ANY (SELECT d.datname FROM pg_catalog.pg_database d)`)
}

// LIKE is the ~~ operator the search_path finds.
func TestCatalogQualifiedReportsLike(t *testing.T) {
	assertReportedOnce(t, `
	SELECT s.slot_name
	FROM pg_catalog.pg_replication_slots s
	WHERE s.slot_name LIKE 'sprite_%'`,
		`LIKE compares through operator "~~", which the search_path resolves`)
	assertPasses(t, `
	SELECT s.slot_name
	FROM pg_catalog.pg_replication_slots s
	WHERE s.slot_name OPERATOR(pg_catalog.~~) 'sprite_%'`)
}

// BETWEEN expands to the >= and <= the search_path finds, and has no
// qualified spelling of its own.
func TestCatalogQualifiedReportsBetween(t *testing.T) {
	assertReportedOnce(t, `
	SELECT c.relname
	FROM pg_catalog.pg_class c
	WHERE c.relpages BETWEEN 1 AND 10`,
		`BETWEEN compares through operator "BETWEEN", which the search_path resolves`)
	assertPasses(t, `
	SELECT c.relname
	FROM pg_catalog.pg_class c
	WHERE c.relpages OPERATOR(pg_catalog.>=) 1
	  AND c.relpages OPERATOR(pg_catalog.<=) 10`)
}

// IS DISTINCT FROM compares with the = the search_path finds. slot_name is
// never null, so the qualified comparison IS NOT TRUE is its exact
// equivalent.
func TestCatalogQualifiedReportsIsDistinctFrom(t *testing.T) {
	assertReportedOnce(t, `
	SELECT s.slot_name
	FROM pg_catalog.pg_replication_slots s
	WHERE s.slot_name IS DISTINCT FROM $1`,
		`IS DISTINCT FROM compares through operator "=", which the search_path resolves`)
	assertPasses(t, `
	SELECT s.slot_name
	FROM pg_catalog.pg_replication_slots s
	WHERE (s.slot_name OPERATOR(pg_catalog.=) $1) IS NOT TRUE`)
}

// A bare relation is the one the search_path finds, in a subquery as much
// as at the top level.
func TestCatalogQualifiedReportsABareRelationInASubquery(t *testing.T) {
	assertReportedOnce(t, `
	SELECT pg_catalog.count(*)
	FROM pg_catalog.pg_replication_slots s
	WHERE EXISTS (
	      SELECT 1
	      FROM pg_database d
	      WHERE d.datname OPERATOR(pg_catalog.=) s.database
	      )`,
		`relation "pg_database" is not qualified`)
}

// A bare operator character is the operator the search_path finds.
func TestCatalogQualifiedReportsABareOperator(t *testing.T) {
	assertReportedOnce(t, `
	SELECT s.slot_name
	FROM pg_catalog.pg_replication_slots s
	WHERE pg_catalog.to_jsonb(s) ->> 'slot_type' OPERATOR(pg_catalog.=) 'logical'`,
		`operator "->>" is not spelled OPERATOR(pg_catalog.…)`)
}

// What the grammar resolves in pg_catalog itself, or that resolves
// through no operator or function at all, passes unqualified: keyword
// types, SQL value functions, the star, parameters, NULL tests, AND, OR,
// and NOT, a common table expression's name, and the caller's own
// relation.
func TestCatalogQualifiedPassesWhatTheGrammarResolvesItself(t *testing.T) {
	assertPasses(t, `
	WITH own_rows AS (
	     SELECT c.slot_name
	     FROM "sprite"."checkpoints" c
	     WHERE c.slot_name IS NOT NULL
	     )
	SELECT s.*, $1::integer, CURRENT_TIMESTAMP, CURRENT_USER
	FROM pg_catalog.pg_replication_slots s
	WHERE NOT s.active
	  AND (s.temporary IS NULL OR s.slot_name OPERATOR(pg_catalog.=) ANY (SELECT o.slot_name FROM own_rows o))`,
		`"sprite"."checkpoints"`)
}

// A simple CASE compares its argument with each WHEN through the =
// the search_path finds.
func TestCatalogQualifiedReportsASimpleCase(t *testing.T) {
	assertReportedOnce(t, `
	SELECT CASE s.slot_type WHEN 'logical' THEN 1 ELSE 0 END
	FROM pg_catalog.pg_replication_slots s`,
		`CASE x WHEN compares x with each WHEN through an unqualified =`)
	assertPasses(t, `
	SELECT CASE WHEN s.slot_type OPERATOR(pg_catalog.=) 'logical' THEN 1 ELSE 0 END
	FROM pg_catalog.pg_replication_slots s`)
}

// JOIN … USING and NATURAL JOIN compare the shared columns through the =
// the search_path finds.
func TestCatalogQualifiedReportsJoinUsingAndNaturalJoin(t *testing.T) {
	const want = `JOIN … USING and NATURAL JOIN compare the shared columns through an unqualified =`
	assertReportedOnce(t, `
	SELECT 1
	FROM pg_catalog.pg_replication_slots s
	JOIN pg_catalog.pg_stat_replication r USING (pid)`, want)
	assertReportedOnce(t, `
	SELECT 1
	FROM pg_catalog.pg_replication_slots s
	NATURAL JOIN pg_catalog.pg_stat_replication r`, want)
}

// A collation is looked up through the search_path unless qualified.
func TestCatalogQualifiedReportsAnUnqualifiedCollation(t *testing.T) {
	assertReportedOnce(t, `
	SELECT s.slot_name COLLATE "C"
	FROM pg_catalog.pg_replication_slots s`,
		`collation "C" is not qualified with pg_catalog`)
	assertPasses(t, `
	SELECT s.slot_name COLLATE pg_catalog."C"
	FROM pg_catalog.pg_replication_slots s`)
}

// ORDER BY … USING names a sort operator the search_path resolves unless
// it is spelled OPERATOR(pg_catalog.…).
func TestCatalogQualifiedReportsOrderByUsing(t *testing.T) {
	assertReportedOnce(t, `
	SELECT s.slot_name
	FROM pg_catalog.pg_replication_slots s
	ORDER BY s.slot_name USING <`,
		`ORDER BY … USING "<" is not spelled OPERATOR(pg_catalog.…)`)
	assertPasses(t, `
	SELECT s.slot_name
	FROM pg_catalog.pg_replication_slots s
	ORDER BY s.slot_name USING OPERATOR(pg_catalog.<)`)
}

// An ALL over a subquery compares through the operator it names.
func TestCatalogQualifiedReportsAllOverASubquery(t *testing.T) {
	assertReportedOnce(t, `
	SELECT 1
	FROM pg_catalog.pg_replication_slots s
	WHERE s.xmin <> ALL (SELECT a.backend_xmin FROM pg_catalog.pg_stat_activity a)`,
		`op ALL (subquery) compares through operator "<>"`)
}

// pg_catalog is the only schema a lookalike cannot be planted ahead of;
// a name in any other schema is reported.
func TestCatalogQualifiedReportsAnotherSchema(t *testing.T) {
	assertReportedOnce(t, `SELECT 1 FROM app.pg_class c`,
		`relation "app.pg_class" is not qualified`)
	assertReportedOnce(t, `SELECT app.to_jsonb(1)`,
		`function "app.to_jsonb" is not qualified`)
	assertReportedOnce(t, `SELECT $1::app.text`,
		`type "app.text" is not qualified`)
}

// A query that does not parse is reported, not passed.
func TestCatalogQualifiedReportsAQueryThatDoesNotParse(t *testing.T) {
	r := &reports{}
	AssertCatalogQualified(r, `SELEC 1`)
	assert.NotEmpty(t, r.got)
}

// The caller's own relation passes spelled plain as well as quoted.
func TestCatalogQualifiedPassesTheOwnRelationSpelledPlain(t *testing.T) {
	assertPasses(t, `SELECT c.slot_name FROM sprite.checkpoints c`, `sprite.checkpoints`)
}

// FOR UPDATE OF names the alias of a relation read in FROM, not a
// relation the search_path resolves.
func TestCatalogQualifiedPassesALockedAlias(t *testing.T) {
	assertPasses(t, `
	SELECT c.slot_name
	FROM "sprite"."checkpoints" c
	FOR UPDATE OF c`,
		`"sprite"."checkpoints"`)
}

// A common table expression's name is in scope after it and, under
// RECURSIVE, in its own body; outside that scope the bare name is a
// relation the search_path resolves.
func TestCatalogQualifiedExcusesACommonTableExpressionOnlyInScope(t *testing.T) {
	assertReportedOnce(t, `
	WITH pg_class AS (SELECT c.oid FROM pg_class c)
	SELECT p.oid FROM pg_class p`,
		`relation "pg_class" is not qualified`)
	assertPasses(t, `
	WITH RECURSIVE chain(n) AS (
	     SELECT 1
	     UNION ALL
	     SELECT chain.n OPERATOR(pg_catalog.+) 1 FROM chain WHERE chain.n OPERATOR(pg_catalog.<) 3
	     ),
	     later AS (SELECT chain.n FROM chain)
	SELECT later.n FROM later`)
}
