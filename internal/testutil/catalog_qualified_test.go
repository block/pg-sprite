package testutil

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
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
	assert.Len(t, bare.got, 1)
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
		`operator "=" (AEXPR_IN) is not spelled OPERATOR(pg_catalog.…)`)
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
		`subquery comparison "" (ANY_SUBLINK) is not spelled OPERATOR(pg_catalog.…)`)
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
		`operator "~~" (AEXPR_LIKE) is not spelled OPERATOR(pg_catalog.…)`)
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
		`operator "BETWEEN" (AEXPR_BETWEEN) is not spelled OPERATOR(pg_catalog.…)`)
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
		`operator "=" (AEXPR_DISTINCT) is not spelled OPERATOR(pg_catalog.…)`)
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
		`operator "->>" (AEXPR_OP) is not spelled OPERATOR(pg_catalog.…)`)
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
