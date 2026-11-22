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

// AssertCatalogQualified fails on any relation, function, or type in query
// not qualified with pg_catalog, the relations in own and the query's own
// common table expressions excepted, and on any operator not spelled
// OPERATOR(pg_catalog.…). It is the text half of CO-9 for a read that runs
// on a caller's pool, whatever its search_path; a lookalike pool proves the
// behaviour.
//
// The query is parsed with the PostgreSQL grammar and every node of the
// parse tree is judged, so a name is caught however it is spelled — quoted,
// spaced from its parenthesis or cast marker, or written as a type's
// constructor — and inside subqueries as well. An operator is any A_Expr,
// which covers the keyword forms that resolve one through the path (IN,
// LIKE, BETWEEN, IS DISTINCT FROM, NULLIF): those parse with an unqualified
// operator name, and BETWEEN with no operator name at all, so they are
// reported however they are written. So are an IN over a subquery, which
// compares with an unqualified =, and an ORDER BY … USING operator. Names
// the grammar itself resolves in pg_catalog, such as the keyword types
// (::integer, ::boolean) and the SQL-standard functions (EXTRACT,
// SUBSTRING), parse already qualified and pass. A relation in own is
// written as the query spells it, quoted as pgx.Identifier.Sanitize quotes
// it or plain.
func AssertCatalogQualified(t catalogT, query string, own ...string) {
	t.Helper()
	tree, err := pgquery.Parse(query)
	if !assert.NoError(t, err, "parse:\n%s", query) {
		return
	}
	root := tree.ProtoReflect()
	ctes := catalogCTENames(root)
	visitCatalogNodes(root, func(msg protoreflect.Message) {
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
		case *pganalyze.A_Expr:
			assert.True(t, catalogQualified(node.GetName()),
				"operator %q (%s) is not spelled OPERATOR(pg_catalog.…) in:\n%s",
				catalogName(node.GetName()), node.GetKind(), query)
		case *pganalyze.SubLink:
			if catalogComparesWithOperator(node) {
				assert.True(t, catalogQualified(node.GetOperName()),
					"subquery comparison %q (%s) is not spelled OPERATOR(pg_catalog.…) in:\n%s",
					catalogName(node.GetOperName()), node.GetSubLinkType(), query)
			}
		case *pganalyze.SortBy:
			if len(node.GetUseOp()) > 0 {
				assert.True(t, catalogQualified(node.GetUseOp()),
					"ORDER BY operator %q is not spelled OPERATOR(pg_catalog.…) in:\n%s",
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

// catalogOwnRelation reports whether rel is one of own, spelled quoted or
// plain.
func catalogOwnRelation(rel *pganalyze.RangeVar, own []string) bool {
	quoted := pgx.Identifier{rel.GetRelname()}
	if rel.GetSchemaname() != "" {
		quoted = pgx.Identifier{rel.GetSchemaname(), rel.GetRelname()}
	}
	return slices.Contains(own, quoted.Sanitize()) || slices.Contains(own, catalogRelationName(rel))
}

// catalogCTEReference reports whether rel names one of the query's common
// table expressions, which resolve before the search_path is consulted.
func catalogCTEReference(rel *pganalyze.RangeVar, ctes []string) bool {
	return rel.GetSchemaname() == "" && slices.Contains(ctes, rel.GetRelname())
}

// catalogCTENames lists the names of every common table expression in the
// tree.
func catalogCTENames(root protoreflect.Message) []string {
	var names []string
	visitCatalogNodes(root, func(msg protoreflect.Message) {
		if cte, ok := msg.Interface().(*pganalyze.CommonTableExpr); ok {
			names = append(names, cte.GetCtename())
		}
	})
	return names
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
// through lists and maps as well as plain fields. Adapted from
// pkg/statement's walkMessages, without the early stop.
func visitCatalogNodes(msg protoreflect.Message, visit func(protoreflect.Message)) {
	visit(msg)
	msg.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.Kind() != protoreflect.MessageKind {
			return true
		}
		switch {
		case field.IsMap():
			if field.MapValue().Kind() == protoreflect.MessageKind {
				value.Map().Range(func(_ protoreflect.MapKey, item protoreflect.Value) bool {
					visitCatalogNodes(item.Message(), visit)
					return true
				})
			}
		case field.IsList():
			list := value.List()
			for i := range list.Len() {
				visitCatalogNodes(list.Get(i).Message(), visit)
			}
		default:
			visitCatalogNodes(value.Message(), visit)
		}
		return true
	})
}
