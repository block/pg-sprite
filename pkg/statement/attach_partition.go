package statement

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	pganalyze "github.com/pganalyze/pg_query_go/v6"
	pgquery "github.com/wasilibs/go-pgquery"
)

// PartitionStrategy is how a partitioned table divides its rows, as a
// partition bound or a partition key spells it.
type PartitionStrategy string

// The partition strategies PostgreSQL has.
const (
	PartitionByRange PartitionStrategy = "range"
	PartitionByList  PartitionStrategy = "list"
	PartitionByHash  PartitionStrategy = "hash"
)

// BoundDatumKind is what one value of a FOR VALUES clause is.
type BoundDatumKind int

// The kinds of a FOR VALUES value.
const (
	// BoundValue is a plain constant: a string, a number, or a boolean.
	BoundValue BoundDatumKind = iota + 1
	// BoundNull is NULL, which only a list bound may hold.
	BoundNull
	// BoundMinValue is a range bound's MINVALUE.
	BoundMinValue
	// BoundMaxValue is a range bound's MAXVALUE.
	BoundMaxValue
	// BoundExpression is anything else: a cast, a function call, an
	// operator, a bit string. The server evaluates it when it attaches, so
	// its value is not known from the statement alone.
	BoundExpression
)

// BoundDatum is one value of a FOR VALUES clause. Value is the constant as
// text, unquoted, and is set only for a BoundValue.
type BoundDatum struct {
	Kind  BoundDatumKind
	Value string
}

// PartitionBound is a FOR VALUES clause, or DEFAULT. Strategy names the
// clause's form; List, Lower and Upper, or Modulus and Remainder are set by
// it.
type PartitionBound struct {
	Default   bool
	Strategy  PartitionStrategy
	List      []BoundDatum
	Lower     []BoundDatum
	Upper     []BoundDatum
	Modulus   int
	Remainder int
}

// AttachPartition is a single-command ALTER TABLE ... ATTACH PARTITION: the
// partitioned table, the table it attaches, each as the grammar spelled it
// (an empty schema when unqualified), and the bound.
type AttachPartition struct {
	ParentSchema string
	Parent       string
	Schema       string
	Name         string
	Bound        PartitionBound
}

// ErrUnexpectedPartitionBound is returned when the grammar produced a
// partition bound or key of a shape this package does not know.
var ErrUnexpectedPartitionBound = errors.New("unexpected partition bound or key shape")

// AttachedPartition returns the ATTACH PARTITION a statement is. ok is false
// for every other statement, including an ALTER TABLE with more than one
// subcommand.
func AttachedPartition(sql string) (AttachPartition, bool, error) {
	node, err := parseSingle(sql)
	if err != nil {
		return AttachPartition{}, false, err
	}
	alter := node.GetAlterTableStmt()
	if alter == nil || len(alter.GetCmds()) != 1 {
		return AttachPartition{}, false, nil
	}
	cmd := alter.GetCmds()[0].GetAlterTableCmd()
	if cmd.GetSubtype() != pganalyze.AlterTableType_AT_AttachPartition {
		return AttachPartition{}, false, nil
	}
	part := cmd.GetDef().GetPartitionCmd()
	bound, err := partitionBound(part.GetBound())
	if err != nil {
		return AttachPartition{}, false, err
	}
	return AttachPartition{
		ParentSchema: alter.GetRelation().GetSchemaname(),
		Parent:       alter.GetRelation().GetRelname(),
		Schema:       part.GetName().GetSchemaname(),
		Name:         part.GetName().GetRelname(),
		Bound:        bound,
	}, true, nil
}

func partitionBound(spec *pganalyze.PartitionBoundSpec) (PartitionBound, error) {
	if spec == nil {
		return PartitionBound{}, fmt.Errorf("%w: ATTACH PARTITION has no bound", ErrUnexpectedPartitionBound)
	}
	if spec.GetIsDefault() {
		return PartitionBound{Default: true}, nil
	}
	switch spec.GetStrategy() {
	case "r":
		return PartitionBound{
			Strategy: PartitionByRange,
			Lower:    boundDatums(spec.GetLowerdatums()),
			Upper:    boundDatums(spec.GetUpperdatums()),
		}, nil
	case "l":
		return PartitionBound{Strategy: PartitionByList, List: boundDatums(spec.GetListdatums())}, nil
	case "h":
		return PartitionBound{
			Strategy:  PartitionByHash,
			Modulus:   int(spec.GetModulus()),
			Remainder: int(spec.GetRemainder()),
		}, nil
	default:
		return PartitionBound{}, fmt.Errorf("%w: partition bound strategy %q", ErrUnexpectedPartitionBound, spec.GetStrategy())
	}
}

func boundDatums(nodes []*pganalyze.Node) []BoundDatum {
	datums := make([]BoundDatum, 0, len(nodes))
	for _, n := range nodes {
		datums = append(datums, boundDatum(n))
	}
	return datums
}

// boundDatum reads one FOR VALUES value. The grammar spells MINVALUE and
// MAXVALUE as a one-field column reference, which the server recognizes by
// its folded name, so a quoted "minvalue" is MINVALUE too.
func boundDatum(n *pganalyze.Node) BoundDatum {
	if ref := n.GetColumnRef(); ref != nil {
		fields := ref.GetFields()
		if len(fields) == 1 {
			switch fields[0].GetString_().GetSval() {
			case "minvalue":
				return BoundDatum{Kind: BoundMinValue}
			case "maxvalue":
				return BoundDatum{Kind: BoundMaxValue}
			}
		}
		return BoundDatum{Kind: BoundExpression}
	}
	c := n.GetAConst()
	if c == nil {
		return BoundDatum{Kind: BoundExpression}
	}
	if c.GetIsnull() {
		return BoundDatum{Kind: BoundNull}
	}
	switch v := c.GetVal().(type) {
	case *pganalyze.A_Const_Sval:
		return BoundDatum{Kind: BoundValue, Value: v.Sval.GetSval()}
	case *pganalyze.A_Const_Ival:
		return BoundDatum{Kind: BoundValue, Value: strconv.FormatInt(int64(v.Ival.GetIval()), 10)}
	case *pganalyze.A_Const_Fval:
		return BoundDatum{Kind: BoundValue, Value: v.Fval.GetFval()}
	case *pganalyze.A_Const_Boolval:
		return BoundDatum{Kind: BoundValue, Value: strconv.FormatBool(v.Boolval.GetBoolval())}
	default:
		return BoundDatum{Kind: BoundExpression}
	}
}

// PartitionKeyElem is one column of a partition key as pg_get_partkeydef
// prints it: a column, by name, or an expression, as the printed SQL text.
// Collated and OpClass report an explicit COLLATE or operator class, which
// the server prints only when they differ from the key's defaults.
type PartitionKeyElem struct {
	Column     string
	Expression string
	Collated   bool
	OpClass    bool
}

// PartitionKey parses a partition key as pg_get_partkeydef prints it, such
// as `RANGE (created_at)` or `LIST (lower(name))`, through the grammar. An
// expression's text is the span of the printed key the parser locates for
// it, and is parsed again on its own to prove it is one whole expression.
func PartitionKey(keydef string) (PartitionStrategy, []PartitionKeyElem, error) {
	const prefix = "CREATE TABLE pg_sprite_partition_key () PARTITION BY "
	sql := prefix + keydef
	node, err := parseSingle(sql)
	if err != nil {
		return "", nil, fmt.Errorf("partition key %q: %w", keydef, err)
	}
	spec := node.GetCreateStmt().GetPartspec()
	if spec == nil {
		return "", nil, fmt.Errorf("%w: partition key %q", ErrUnexpectedPartitionBound, keydef)
	}
	var strategy PartitionStrategy
	switch spec.GetStrategy() {
	case pganalyze.PartitionStrategy_PARTITION_STRATEGY_RANGE:
		strategy = PartitionByRange
	case pganalyze.PartitionStrategy_PARTITION_STRATEGY_LIST:
		strategy = PartitionByList
	case pganalyze.PartitionStrategy_PARTITION_STRATEGY_HASH:
		strategy = PartitionByHash
	default:
		return "", nil, fmt.Errorf("%w: partition key %q has strategy %s", ErrUnexpectedPartitionBound, keydef, spec.GetStrategy())
	}
	params := spec.GetPartParams()
	// The key list ends at the closing parenthesis, the statement's last
	// character.
	end := strings.LastIndexByte(sql, ')')
	elems := make([]PartitionKeyElem, 0, len(params))
	for i, p := range params {
		pe := p.GetPartitionElem()
		if pe == nil {
			return "", nil, fmt.Errorf("%w: partition key %q", ErrUnexpectedPartitionBound, keydef)
		}
		elem := PartitionKeyElem{Column: pe.GetName(), Collated: len(pe.GetCollation()) > 0, OpClass: len(pe.GetOpclass()) > 0}
		if pe.GetExpr() != nil {
			stop := end
			if i+1 < len(params) {
				stop = int(params[i+1].GetPartitionElem().GetLocation())
			}
			text, err := keyExpressionText(sql, int(pe.GetLocation()), stop, elem)
			if err != nil {
				return "", nil, fmt.Errorf("partition key %q: %w", keydef, err)
			}
			elem.Expression = text
		}
		elems = append(elems, elem)
	}
	return strategy, elems, nil
}

// keyExpressionText cuts one key expression from the printed key, between
// its own location and the next element's (or the closing parenthesis).
// Only an expression with no COLLATE or operator class is cut, so the span
// holds the expression and the list's separating comma alone.
func keyExpressionText(sql string, start, stop int, elem PartitionKeyElem) (string, error) {
	if elem.Collated || elem.OpClass {
		return "", nil
	}
	if start < 0 || stop > len(sql) || start >= stop {
		return "", fmt.Errorf("%w: key expression at %d..%d", ErrUnexpectedPartitionBound, start, stop)
	}
	text := strings.TrimSpace(sql[start:stop])
	text = strings.TrimSpace(strings.TrimSuffix(text, ","))
	tree, err := pgquery.Parse("SELECT " + text)
	if err != nil {
		return "", fmt.Errorf("%w: key expression %q does not parse alone: %w", ErrUnexpectedPartitionBound, text, err)
	}
	stmts := tree.GetStmts()
	if len(stmts) != 1 || len(stmts[0].GetStmt().GetSelectStmt().GetTargetList()) != 1 {
		return "", fmt.Errorf("%w: key expression %q is not one expression", ErrUnexpectedPartitionBound, text)
	}
	return text, nil
}
