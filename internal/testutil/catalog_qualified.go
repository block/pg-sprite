package testutil

import (
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	pganalyze "github.com/pganalyze/pg_query_go/v6"
	"github.com/stretchr/testify/assert"
	pgquery "github.com/wasilibs/go-pgquery"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// catalogT is the part of *testing.T AssertCatalogQualified uses, so its
// own test can record a report rather than fail.
type catalogT interface {
	Helper()
	Errorf(format string, args ...any)
}

// AssertCatalogQualified fails on any relation, function, type, or
// collation in query not qualified with pg_catalog, the relations in own
// and the query's own common table expressions excepted, and on any
// operator not spelled OPERATOR(pg_catalog.…). It is the SQL half of CO-9
// for a read that runs on a caller's pool, whatever its search_path; a
// lookalike pool proves the behaviour.
//
// The query is parsed with the PostgreSQL grammar and every node of the
// parse tree is judged, so a name is caught however it is spelled — quoted,
// spaced from its parenthesis or cast marker, or written as a type's
// constructor — and inside subqueries as well. An operator is any A_Expr,
// which covers the keyword forms that resolve one through the path (IN,
// LIKE, BETWEEN, IS DISTINCT FROM, NULLIF): those parse with an unqualified
// operator name, and BETWEEN with no operator name at all, so they are
// reported however they are written. So are the forms whose = the analyzer
// builds without any operator node — an IN over a subquery, a simple
// CASE x WHEN, and a JOIN … USING or NATURAL JOIN — and an ORDER BY … USING
// operator. Names the grammar itself resolves in pg_catalog, such as the
// keyword types (::integer, ::boolean) and the SQL-standard functions
// (EXTRACT, SUBSTRING), parse already qualified and pass. A common table
// expression's name is excused only where that expression is in scope, and
// the relations a FOR UPDATE OF clause names are aliases, judged where the
// query reads them. A relation in own is written as the query spells it,
// quoted as pgx.Identifier.Sanitize quotes it or plain.
func AssertCatalogQualified(t catalogT, query string, own ...string) {
	t.Helper()
	tree, err := pgquery.Parse(query)
	if !assert.NoError(t, err, "parse:\n%s", query) {
		return
	}
	visitCatalogNodes(tree.ProtoReflect(), nil, func(msg protoreflect.Message, ctes []string) {
		switch node := msg.Interface().(type) {
		case *pganalyze.RangeVar:
			if catalogOwnRelation(node, own) || catalogCTEReference(node, ctes) {
				return
			}
			assert.True(t, node.GetSchemaname() == "pg_catalog",
				"relation %q is not qualified with pg_catalog in:\n%s", catalogRelationName(node), query)
		case *pganalyze.FuncCall:
			assert.True(t, catalogQualified(node.GetFuncname()),
				"function %q is not qualified with pg_catalog in:\n%s", catalogName(node.GetFuncname()), query)
		case *pganalyze.TypeName:
			assert.True(t, catalogQualified(node.GetNames()),
				"type %q is not qualified with pg_catalog in:\n%s", catalogName(node.GetNames()), query)
		case *pganalyze.CollateClause:
			assert.True(t, catalogQualified(node.GetCollname()),
				"collation %q is not qualified with pg_catalog in:\n%s", catalogName(node.GetCollname()), query)
		case *pganalyze.A_Expr:
			assert.True(t, catalogQualified(node.GetName()), "%s in:\n%s", catalogOperatorReport(node), query)
		case *pganalyze.SubLink:
			if catalogComparesWithOperator(node) {
				assert.True(t, catalogQualified(node.GetOperName()), "%s in:\n%s", catalogSubLinkReport(node), query)
			}
		case *pganalyze.CaseExpr:
			assert.True(t, node.GetArg() == nil,
				"CASE x WHEN compares x with each WHEN through an unqualified =; write CASE WHEN x OPERATOR(pg_catalog.=) … in:\n%s", query)
		case *pganalyze.JoinExpr:
			assert.True(t, !node.GetIsNatural() && len(node.GetUsingClause()) == 0,
				"JOIN … USING and NATURAL JOIN compare the shared columns through an unqualified =; write ON a.col OPERATOR(pg_catalog.=) b.col in:\n%s", query)
		case *pganalyze.SortBy:
			if len(node.GetUseOp()) > 0 {
				assert.True(t, catalogQualified(node.GetUseOp()),
					"ORDER BY … USING %q is not spelled OPERATOR(pg_catalog.…) in:\n%s",
					catalogName(node.GetUseOp()), query)
			}
		}
	})
}

// catalogQualified reports whether names is a two-part name in pg_catalog.
func catalogQualified(names []*pganalyze.Node) bool {
	return len(names) == 2 && names[0].GetString_().GetSval() == "pg_catalog"
}

// catalogName renders a parsed name list dotted, for a report.
func catalogName(names []*pganalyze.Node) string {
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name.GetString_().GetSval())
	}
	return strings.Join(parts, ".")
}

// catalogRelationName renders a relation dotted, for a report.
func catalogRelationName(rel *pganalyze.RangeVar) string {
	if rel.GetSchemaname() == "" {
		return rel.GetRelname()
	}
	return rel.GetSchemaname() + "." + rel.GetRelname()
}

// catalogKeywordForms names, as a query spells them, the keyword forms
// that compare through an operator the path resolves.
var catalogKeywordForms = map[pganalyze.A_Expr_Kind]string{
	pganalyze.A_Expr_Kind_AEXPR_OP_ANY:          "op ANY (array)",
	pganalyze.A_Expr_Kind_AEXPR_OP_ALL:          "op ALL (array)",
	pganalyze.A_Expr_Kind_AEXPR_DISTINCT:        "IS DISTINCT FROM",
	pganalyze.A_Expr_Kind_AEXPR_NOT_DISTINCT:    "IS NOT DISTINCT FROM",
	pganalyze.A_Expr_Kind_AEXPR_NULLIF:          "NULLIF",
	pganalyze.A_Expr_Kind_AEXPR_IN:              "IN (list)",
	pganalyze.A_Expr_Kind_AEXPR_LIKE:            "LIKE",
	pganalyze.A_Expr_Kind_AEXPR_ILIKE:           "ILIKE",
	pganalyze.A_Expr_Kind_AEXPR_SIMILAR:         "SIMILAR TO",
	pganalyze.A_Expr_Kind_AEXPR_BETWEEN:         "BETWEEN",
	pganalyze.A_Expr_Kind_AEXPR_NOT_BETWEEN:     "NOT BETWEEN",
	pganalyze.A_Expr_Kind_AEXPR_BETWEEN_SYM:     "BETWEEN SYMMETRIC",
	pganalyze.A_Expr_Kind_AEXPR_NOT_BETWEEN_SYM: "NOT BETWEEN SYMMETRIC",
}

// catalogOperatorReport says what an unqualified operator expression
// resolves through the path and how to write it qualified.
func catalogOperatorReport(expr *pganalyze.A_Expr) string {
	form, keyword := catalogKeywordForms[expr.GetKind()]
	if !keyword {
		return "operator " + quoteCatalogName(expr.GetName()) + " is not spelled OPERATOR(pg_catalog.…)"
	}
	return form + " compares through operator " + quoteCatalogName(expr.GetName()) +
		", which the search_path resolves; write the comparison with OPERATOR(pg_catalog.…)"
}

// catalogSubLinkReport says what an unqualified subquery comparison
// resolves through the path and how to write it qualified.
func catalogSubLinkReport(link *pganalyze.SubLink) string {
	switch {
	case link.GetSubLinkType() == pganalyze.SubLinkType_ANY_SUBLINK && len(link.GetOperName()) == 0:
		return "IN (subquery) compares through an unqualified =; write x OPERATOR(pg_catalog.=) ANY (subquery)"
	case link.GetSubLinkType() == pganalyze.SubLinkType_ALL_SUBLINK:
		return "op ALL (subquery) compares through operator " + quoteCatalogName(link.GetOperName()) +
			", which the search_path resolves; write x OPERATOR(pg_catalog.…) ALL (subquery)"
	case link.GetSubLinkType() == pganalyze.SubLinkType_ROWCOMPARE_SUBLINK:
		return "a row compared with a subquery compares through operator " + quoteCatalogName(link.GetOperName()) +
			", which the search_path resolves; write it with OPERATOR(pg_catalog.…)"
	default:
		return "op ANY (subquery) compares through operator " + quoteCatalogName(link.GetOperName()) +
			", which the search_path resolves; write x OPERATOR(pg_catalog.…) ANY (subquery)"
	}
}

// quoteCatalogName renders a parsed name list dotted and quoted, for a
// report.
func quoteCatalogName(names []*pganalyze.Node) string {
	return `"` + catalogName(names) + `"`
}

// catalogOwnRelation reports whether rel is one of own, spelled quoted or
// plain.
func catalogOwnRelation(rel *pganalyze.RangeVar, own []string) bool {
	quoted := pgx.Identifier{rel.GetRelname()}
	if rel.GetSchemaname() != "" {
		quoted = pgx.Identifier{rel.GetSchemaname(), rel.GetRelname()}
	}
	return slices.Contains(own, quoted.Sanitize()) || slices.Contains(own, catalogRelationName(rel))
}

// catalogCTEReference reports whether rel names a common table expression
// in scope, which resolves before the search_path is consulted.
func catalogCTEReference(rel *pganalyze.RangeVar, ctes []string) bool {
	return rel.GetSchemaname() == "" && slices.Contains(ctes, rel.GetRelname())
}

// catalogComparesWithOperator reports whether a subquery link compares its
// test expression with an operator: ANY and ALL, which an IN over a
// subquery parses as with no operator name, and a row comparison. EXISTS,
// scalar, and ARRAY subqueries compare nothing.
func catalogComparesWithOperator(link *pganalyze.SubLink) bool {
	switch link.GetSubLinkType() {
	case pganalyze.SubLinkType_ANY_SUBLINK, pganalyze.SubLinkType_ALL_SUBLINK, pganalyze.SubLinkType_ROWCOMPARE_SUBLINK:
		return true
	default:
		return false
	}
}

// visitCatalogNodes calls visit on msg and every message nested in it,
// through lists and maps as well as plain fields, with the names of the
// common table expressions in scope at each. A statement's WITH puts its
// expressions in scope for the rest of the statement; inside an
// expression's own body only the ones before it are, unless the WITH is
// RECURSIVE. A FOR UPDATE OF clause is not walked: it names aliases of
// relations judged where the query reads them.
func visitCatalogNodes(msg protoreflect.Message, ctes []string, visit func(protoreflect.Message, []string)) {
	if _, locking := msg.Interface().(*pganalyze.LockingClause); locking {
		return
	}
	withField := msg.Descriptor().Fields().ByName("with_clause")
	if withField != nil && msg.Has(withField) {
		with, _ := msg.Get(withField).Message().Interface().(*pganalyze.WithClause)
		ctes = visitCatalogWith(with, ctes, visit)
	}
	visit(msg, ctes)
	msg.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.Kind() != protoreflect.MessageKind || field == withField {
			return true
		}
		switch {
		case field.IsMap():
			if field.MapValue().Kind() == protoreflect.MessageKind {
				value.Map().Range(func(_ protoreflect.MapKey, item protoreflect.Value) bool {
					visitCatalogNodes(item.Message(), ctes, visit)
					return true
				})
			}
		case field.IsList():
			list := value.List()
			for i := range list.Len() {
				visitCatalogNodes(list.Get(i).Message(), ctes, visit)
			}
		default:
			visitCatalogNodes(value.Message(), ctes, visit)
		}
		return true
	})
}

// visitCatalogWith walks each common table expression of with with the
// names in scope inside its body, and returns the names in scope for the
// rest of the statement.
func visitCatalogWith(with *pganalyze.WithClause, outer []string, visit func(protoreflect.Message, []string)) []string {
	all := slices.Clone(outer)
	for _, node := range with.GetCtes() {
		all = append(all, node.GetCommonTableExpr().GetCtename())
	}
	before := slices.Clone(outer)
	for _, node := range with.GetCtes() {
		cte := node.GetCommonTableExpr()
		inBody := before
		if with.GetRecursive() {
			inBody = all
		}
		visitCatalogNodes(cte.ProtoReflect(), inBody, visit)
		before = append(before, cte.GetCtename())
	}
	return all
}
