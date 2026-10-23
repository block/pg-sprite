package statement

import (
	"cmp"
	"fmt"
	"slices"

	pganalyze "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// QualifiedName is a name an expression spells, with the schema it was
// qualified with or the empty string when the search_path is left to
// resolve it. For a name of more than two parts the schema is the parts
// before the last, joined as written.
type QualifiedName struct {
	Schema string
	Name   string
}

// String renders the name as the expression would spell it.
func (n QualifiedName) String() string {
	if n.Schema == "" {
		return n.Name
	}
	return n.Schema + "." + n.Name
}

func compareQualifiedNames(a, b QualifiedName) int {
	if c := cmp.Compare(a.Schema, b.Schema); c != 0 {
		return c
	}
	return cmp.Compare(a.Name, b.Name)
}

// ConversionNames are the names a USING expression leaves to the catalog to
// resolve, by kind, each list sorted and without repeats. They are what a
// resolution of the expression depends on: the function names it calls,
// the operator names it applies, the type names it casts to, and the
// collations it names. The lists say which names the expression spells,
// not which catalog objects they resolved to; resolving them is the
// server's job, and what the server would resolve them to is what a
// fingerprint of the catalog entries under these names pins.
type ConversionNames struct {
	Functions  []QualifiedName
	Operators  []QualifiedName
	Types      []QualifiedName
	Collations []QualifiedName
}

// Empty reports whether the expression spelled no name of any kind.
func (n ConversionNames) Empty() bool {
	return len(n.Functions) == 0 && len(n.Operators) == 0 && len(n.Types) == 0 && len(n.Collations) == 0
}

// ConversionNamesOf reads the names out of a USING expression as
// ColumnConversions rendered it: the expression is parsed as the one target
// of a SELECT and its tree walked for every function call, operator, type
// cast, and COLLATE clause. The walk reads the tree's shape, not its
// meaning, so it reports what the grammar wrote down and nothing the
// server adds while it resolves: a BETWEEN is the two comparison operators
// the server rewrites it to, a CASE with an operand compares with the
// unqualified `=`, and the grammar's own spellings — `integer` parsed as
// pg_catalog.int4, LIKE as the `~~` operator — appear as the grammar
// spells them. What the server resolves without a name the expression
// spelled — an implicit cast between two types, the comparison GREATEST
// and LEAST pick for a type — is not in the result.
func ConversionNamesOf(expr string) (ConversionNames, error) {
	node, err := parseSingle(selectPrefix + expr)
	if err != nil {
		return ConversionNames{}, fmt.Errorf("parse USING expression %q: %w", expr, err)
	}
	var names ConversionNames
	walkMessages(node.ProtoReflect(), func(msg protoreflect.Message) bool {
		switch m := msg.Interface().(type) {
		case *pganalyze.FuncCall:
			names.Functions = append(names.Functions, qualifiedName(m.GetFuncname()))
		case *pganalyze.A_Expr:
			names.Operators = append(names.Operators, operatorNames(m)...)
		case *pganalyze.TypeName:
			names.Types = append(names.Types, qualifiedName(m.GetNames()))
		case *pganalyze.CollateClause:
			names.Collations = append(names.Collations, qualifiedName(m.GetCollname()))
		case *pganalyze.CaseExpr:
			if m.GetArg() != nil {
				names.Operators = append(names.Operators, QualifiedName{Name: "="})
			}
		}
		return true
	})
	names.Functions = sortedNames(names.Functions)
	names.Operators = sortedNames(names.Operators)
	names.Types = sortedNames(names.Types)
	names.Collations = sortedNames(names.Collations)
	return names, nil
}

// operatorNames is the operator names an A_Expr node resolves: its own
// name for every kind but the BETWEEN family, which the server rewrites
// into the `>=` and `<=` of the operand's type and never resolves as the
// keyword the grammar records.
func operatorNames(expr *pganalyze.A_Expr) []QualifiedName {
	switch expr.GetKind() {
	case pganalyze.A_Expr_Kind_AEXPR_BETWEEN, pganalyze.A_Expr_Kind_AEXPR_NOT_BETWEEN,
		pganalyze.A_Expr_Kind_AEXPR_BETWEEN_SYM, pganalyze.A_Expr_Kind_AEXPR_NOT_BETWEEN_SYM:
		return []QualifiedName{{Name: ">="}, {Name: "<="}}
	default:
		return []QualifiedName{qualifiedName(expr.GetName())}
	}
}

// qualifiedName reads a grammar name list — String nodes, the last of them
// the name and the ones before it the schema — into a QualifiedName.
func qualifiedName(parts []*pganalyze.Node) QualifiedName {
	spelled := make([]string, 0, len(parts))
	for _, part := range parts {
		spelled = append(spelled, part.GetString_().GetSval())
	}
	if len(spelled) == 0 {
		return QualifiedName{}
	}
	name := QualifiedName{Name: spelled[len(spelled)-1]}
	for i, s := range spelled[:len(spelled)-1] {
		if i > 0 {
			name.Schema += "."
		}
		name.Schema += s
	}
	return name
}

// sortedNames sorts names and drops repeats, so two expressions that spell
// the same names in another order or another number of times read the
// same.
func sortedNames(names []QualifiedName) []QualifiedName {
	if len(names) == 0 {
		return nil
	}
	slices.SortFunc(names, compareQualifiedNames)
	return slices.Compact(names)
}

// walkMessages visits msg and, in field order, every message nested in it
// — through lists and maps as well as plain fields — calling visit on each.
// A visit that returns false ends the walk.
func walkMessages(msg protoreflect.Message, visit func(protoreflect.Message) bool) bool {
	if !visit(msg) {
		return false
	}
	proceed := true
	msg.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.Kind() != protoreflect.MessageKind {
			return true
		}
		switch {
		case field.IsMap():
			if field.MapValue().Kind() == protoreflect.MessageKind {
				value.Map().Range(func(_ protoreflect.MapKey, item protoreflect.Value) bool {
					proceed = walkMessages(item.Message(), visit)
					return proceed
				})
			}
		case field.IsList():
			list := value.List()
			for i := 0; i < list.Len() && proceed; i++ {
				proceed = walkMessages(list.Get(i).Message(), visit)
			}
		default:
			proceed = walkMessages(value.Message(), visit)
		}
		return proceed
	})
	return proceed
}
