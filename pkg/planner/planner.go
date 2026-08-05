// Package planner classifies schema-change statements: for each operation
// it decides whether PostgreSQL can run it online natively (possibly via a
// safer idiom it suggests), whether it needs the engine's copy-and-swap
// path, or whether it is refused. The mapping is the "Needs copy-and-swap?"
// column of docs/postgres-online-ddl-reference.md, applied conservatively:
// anything the planner cannot prove safe routes to copy-and-swap or refuse.
// Classification predicts; executors keep their own protections regardless.
//
// In MySQL terms, the planner is PostgreSQL's missing ALGORITHM= / LOCK=
// declaration: MySQL lets authors assert the cost bracket
// (INSTANT/INPLACE/COPY) and the lock impact (NONE/SHARED/EXCLUSIVE) and
// fails closed; PostgreSQL has no such clause, so the planner proves both
// dimensions before execution and routes to the safest sequence that exists.
package planner

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/statement"
)

// Route is where an operation is sent.
type Route string

// The three routes.
const (
	// RouteNative: PostgreSQL runs it online natively — directly or via
	// the safer idiom in Decision.SaferSQL.
	RouteNative Route = "native"
	// RouteCopyAndSwap: needs a table rewrite; only the engine's shadow
	// copy + cutover can do it online.
	RouteCopyAndSwap Route = "copy-and-swap"
	// RouteRefuse: no known safe path; not executed.
	RouteRefuse Route = "refuse"
)

// worse orders routes for aggregation: refuse > copy-and-swap > native.
func worse(a, b Route) Route {
	rank := map[Route]int{RouteNative: 0, RouteCopyAndSwap: 1, RouteRefuse: 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// Reason is the typed cause of a routing decision; automation branches on
// it, never on prose.
type Reason string

// The reasons a decision can carry.
const (
	// ReasonMetadataOnly: a brief ACCESS EXCLUSIVE catalog change, no scan
	// and no rewrite.
	ReasonMetadataOnly Reason = "metadata-only"
	// ReasonOnlineIdiom: already the safe native form (CONCURRENTLY,
	// NOT VALID, VALIDATE, USING INDEX).
	ReasonOnlineIdiom Reason = "online-idiom"
	// ReasonFastDefault: ADD COLUMN with a constant default — the catalog
	// stores the default, no rewrite (PG 11+).
	ReasonFastDefault Reason = "fast-default"
	// ReasonBinaryCoercible: a type change PostgreSQL relabels without a
	// rewrite (widen varchar, varchar to text, widen numeric precision).
	ReasonBinaryCoercible Reason = "binary-coercible"
	// ReasonSaferIdiom: native, but the submitted form blocks; SaferSQL
	// carries the online rewrite when one can be constructed.
	ReasonSaferIdiom Reason = "safer-idiom"
	// ReasonVolatileDefault: ADD COLUMN whose default the planner cannot
	// prove constant — PostgreSQL rewrites the table.
	ReasonVolatileDefault Reason = "volatile-default"
	// ReasonGeneratedStored: adding a stored generated column computes
	// every row — a full rewrite.
	ReasonGeneratedStored Reason = "generated-stored"
	// ReasonTypeRewrite: a type conversion PostgreSQL cannot relabel —
	// rewrite plus reindex.
	ReasonTypeRewrite Reason = "type-rewrite"
	// ReasonRelocation: SET TABLESPACE moves the heap — a rewrite-scale
	// copy.
	ReasonRelocation Reason = "relocation"
	// ReasonUnsupportedOperation: the planner does not recognize the
	// operation or knows no safe path for it.
	ReasonUnsupportedOperation Reason = "unsupported-operation"
)

// Decision is the classification of one operation.
type Decision struct {
	// Operation is the operator-facing label (display only).
	Operation string `json:"operation"`
	// Route is where the operation goes.
	Route Route `json:"route"`
	// Reason is why.
	Reason Reason `json:"reason"`
	// SaferSQL is the ordered native sequence to run instead of the
	// submitted form, present only for safer-idiom decisions where the
	// planner could construct it.
	SaferSQL []string `json:"safer_sql,omitempty"`
}

// Plan is the classification of one statement: one decision per operation
// and the aggregate route (the worst of its decisions — one rewrite makes
// the whole statement a copy, one refusal refuses it).
type Plan struct {
	// Statement is the submitted SQL.
	Statement string `json:"statement"`
	// Route is the aggregate route.
	Route Route `json:"route"`
	// Decisions are the per-operation classifications, in statement order.
	Decisions []Decision `json:"decisions"`
}

// Facts are introspected properties of the live table that sharpen
// classification. The zero value is valid: with no facts the planner is
// strictly more conservative (every type change becomes copy-and-swap).
type Facts struct {
	// ColumnTypes maps a column name to its live type as rendered by
	// PostgreSQL's format_type (e.g. "character varying(50)").
	ColumnTypes map[string]string
}

// Classify parses one statement and routes each of its operations. A parse
// failure is an error; an unrecognized operation is not — it comes back as
// a refuse decision so the caller can render the whole plan.
func Classify(sql string, facts Facts) (Plan, error) {
	st, err := statement.ParseOne(sql)
	if err != nil {
		return Plan{}, err
	}
	ops, err := statement.ParseOps(sql)
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{Statement: sql, Route: RouteNative}
	// Safer rewrites are only constructed for single-operation statements:
	// a partial rewrite of a multi-operation ALTER would be misleading.
	single := len(ops) == 1
	for _, op := range ops {
		d := classifyOp(op, st, facts, sql, single)
		plan.Route = worse(plan.Route, d.Route)
		plan.Decisions = append(plan.Decisions, d)
	}
	return plan, nil
}

// classifyOp routes one operation per the reference table.
func classifyOp(op statement.Op, st statement.Statement, facts Facts, sql string, single bool) Decision {
	d := Decision{Operation: op.Describe()}
	switch op.Kind {
	case statement.OpCreateTable:
		// A new table has no readers to lock out.
		d.Route, d.Reason = RouteNative, ReasonMetadataOnly

	case statement.OpAddColumn:
		switch {
		case op.GeneratedStored:
			d.Route, d.Reason = RouteCopyAndSwap, ReasonGeneratedStored
		case op.Default == statement.DefaultExpression:
			d.Route, d.Reason = RouteCopyAndSwap, ReasonVolatileDefault
		case op.Default == statement.DefaultConstant:
			d.Route, d.Reason = RouteNative, ReasonFastDefault
		default:
			d.Route, d.Reason = RouteNative, ReasonMetadataOnly
		}

	case statement.OpDropColumn, statement.OpSetDefault, statement.OpDropDefault,
		statement.OpDropNotNull, statement.OpRenameColumn, statement.OpRenameTable,
		statement.OpRenameIndex, statement.OpSetColumnOptions, statement.OpSetRelOptions,
		statement.OpSetSchema, statement.OpDropConstraint:
		d.Route, d.Reason = RouteNative, ReasonMetadataOnly

	case statement.OpAlterColumnType:
		d.Route, d.Reason = classifyTypeChange(op, facts)

	case statement.OpSetNotNull:
		// Native pattern: prove the invariant with a NOT VALID CHECK plus
		// an online VALIDATE, then SET NOT NULL is a catalog flip (PG 12+).
		d.Route, d.Reason = RouteNative, ReasonSaferIdiom
		if single {
			d.SaferSQL = setNotNullSequence(st, op.Column)
		}

	case statement.OpSetTablespace:
		d.Route, d.Reason = RouteCopyAndSwap, ReasonRelocation

	case statement.OpAddConstraint:
		d = classifyAddConstraint(op, st, sql, single)

	case statement.OpValidateConstraint:
		d.Route, d.Reason = RouteNative, ReasonOnlineIdiom

	case statement.OpAttachPartition:
		// Native pattern: pre-add a validated CHECK matching the bound on
		// the child to skip the attach-time scan. The planner cannot
		// construct that CHECK, so it routes native without a rewrite.
		d.Route, d.Reason = RouteNative, ReasonSaferIdiom

	case statement.OpDetachPartition:
		d = concurrentlyDecision(d, op.Concurrent, sql, single)

	case statement.OpCreateIndex, statement.OpDropIndex, statement.OpReindex:
		d = concurrentlyDecision(d, op.Concurrent, sql, single)

	default:
		d.Route, d.Reason = RouteRefuse, ReasonUnsupportedOperation
	}
	return d
}

// concurrentlyDecision routes an operation that is online in its
// CONCURRENTLY form: already concurrent is the idiom; otherwise native with
// the concurrent rewrite as the safer sequence.
func concurrentlyDecision(d Decision, concurrent bool, sql string, single bool) Decision {
	if concurrent {
		d.Route, d.Reason = RouteNative, ReasonOnlineIdiom
		return d
	}
	d.Route, d.Reason = RouteNative, ReasonSaferIdiom
	if single {
		if safer, err := statement.Concurrently(sql); err == nil {
			d.SaferSQL = []string{safer}
		}
	}
	return d
}

// classifyAddConstraint routes ADD CONSTRAINT per constraint family.
func classifyAddConstraint(op statement.Op, st statement.Statement, sql string, single bool) Decision {
	d := Decision{Operation: op.Describe()}
	switch {
	case op.UsingIndex, op.NotValid:
		// Already the safe pattern's cheap step.
		d.Route, d.Reason = RouteNative, ReasonOnlineIdiom

	case op.Constraint == statement.ConstraintPrimaryKey,
		op.Constraint == statement.ConstraintUnique:
		// Direct ADD PK/UNIQUE builds its index under ACCESS EXCLUSIVE;
		// the safer sequence builds it concurrently and attaches it.
		d.Route, d.Reason = RouteNative, ReasonSaferIdiom
		if single && len(op.Columns) > 0 {
			d.SaferSQL = usingIndexSequence(st, op)
		}

	case op.Constraint == statement.ConstraintCheck,
		op.Constraint == statement.ConstraintForeignKey:
		// Direct ADD CHECK/FK validates under ACCESS EXCLUSIVE; the safer
		// sequence is NOT VALID plus an online VALIDATE.
		d.Route, d.Reason = RouteNative, ReasonSaferIdiom
		if single {
			if notValid, name, err := statement.AddNotValid(sql); err == nil {
				d.SaferSQL = []string{
					notValid,
					"ALTER TABLE " + tableIdent(st) + " VALIDATE CONSTRAINT " + pgx.Identifier{name}.Sanitize(),
				}
			}
		}

	case op.Constraint == statement.ConstraintNotNull:
		d.Route, d.Reason = RouteNative, ReasonSaferIdiom

	default:
		// EXCLUDE and anything unrecognized: no online pattern exists.
		d.Route, d.Reason = RouteRefuse, ReasonUnsupportedOperation
	}
	return d
}

// classifyTypeChange routes ALTER COLUMN TYPE: binary-coercible changes are
// a brief catalog relabel; everything else — or anything the planner cannot
// verify against live column facts — is a rewrite.
func classifyTypeChange(op statement.Op, facts Facts) (Route, Reason) {
	if op.HasUsing {
		return RouteCopyAndSwap, ReasonTypeRewrite
	}
	oldType, ok := facts.ColumnTypes[op.Column]
	if !ok {
		return RouteCopyAndSwap, ReasonTypeRewrite
	}
	if binaryCoercible(parseTypeText(oldType), typeShape{name: normalizeTypeName(op.NewType), mods: op.NewTypeMods}) {
		return RouteNative, ReasonBinaryCoercible
	}
	return RouteCopyAndSwap, ReasonTypeRewrite
}

// typeShape is a normalized type family plus its modifiers, comparable
// across the grammar's spelling and format_type's rendering.
type typeShape struct {
	name string
	mods []int32
}

// binaryCoercible reports whether changing old to new is a relabel
// PostgreSQL performs without a rewrite or scan. The rules are the
// reference table's rows, deliberately narrow: widening varchar, varchar to
// text, and widening numeric precision at the same scale. Anything not
// provably on this list is not coercible.
func binaryCoercible(old, next typeShape) bool {
	if old.name == "" || next.name == "" {
		return false
	}
	if old.name == next.name && int32sEqual(old.mods, next.mods) {
		return true // no-op relabel
	}
	switch old.name {
	case "varchar":
		if next.name == "text" {
			return true
		}
		if next.name != "varchar" {
			return false
		}
		if len(next.mods) == 0 {
			return true // dropping the length bound
		}
		return len(old.mods) == 1 && len(next.mods) == 1 && next.mods[0] >= old.mods[0]
	case "numeric":
		if next.name != "numeric" {
			return false
		}
		if len(next.mods) == 0 {
			return true // dropping the precision bound
		}
		return len(old.mods) == 2 && len(next.mods) == 2 &&
			next.mods[1] == old.mods[1] && next.mods[0] >= old.mods[0]
	default:
		return false
	}
}

// int32sEqual reports element-wise equality.
func int32sEqual(a, b []int32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// normalizeTypeName maps spelling variants of the families binaryCoercible
// knows onto one name. Unknown names pass through unchanged (and will never
// match a rule).
func normalizeTypeName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "varchar", "character varying":
		return "varchar"
	case "numeric", "decimal":
		return "numeric"
	case "text":
		return "text"
	default:
		return strings.ToLower(strings.TrimSpace(name))
	}
}

// parseTypeText splits a format_type rendering ("character varying(50)",
// "numeric(10,2)", "text") into its normalized shape. This reads catalog
// output, not SQL — format_type's rendering is stable.
func parseTypeText(s string) typeShape {
	name, rest, found := strings.Cut(s, "(")
	shape := typeShape{name: normalizeTypeName(name)}
	if !found {
		return shape
	}
	rest, _, found = strings.Cut(rest, ")")
	if !found {
		return typeShape{}
	}
	for part := range strings.SplitSeq(rest, ",") {
		n, err := strconv.ParseInt(strings.TrimSpace(part), 10, 32)
		if err != nil {
			return typeShape{}
		}
		shape.mods = append(shape.mods, int32(n))
	}
	return shape
}

// tableIdent renders the statement's target table as a quoted identifier,
// schema-qualified when the statement was.
func tableIdent(st statement.Statement) string {
	if st.Schema != "" {
		return pgx.Identifier{st.Schema, st.Table}.Sanitize()
	}
	return pgx.Identifier{st.Table}.Sanitize()
}

// setNotNullSequence is the native four-step SET NOT NULL pattern: prove
// the invariant online with a NOT VALID CHECK, flip the column, drop the
// scaffold.
func setNotNullSequence(st statement.Statement, column string) []string {
	table := tableIdent(st)
	conName := fmt.Sprintf("%s_%s_not_null", st.Table, column)
	con := pgx.Identifier{conName}.Sanitize()
	col := pgx.Identifier{column}.Sanitize()
	return []string{
		fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s IS NOT NULL) NOT VALID", table, con, col),
		fmt.Sprintf("ALTER TABLE %s VALIDATE CONSTRAINT %s", table, con),
		fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET NOT NULL", table, col),
		fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s", table, con),
	}
}

// usingIndexSequence is the native two-step ADD PRIMARY KEY / UNIQUE
// pattern: build the unique index concurrently, then attach it as the
// constraint under a brief lock.
func usingIndexSequence(st statement.Statement, op statement.Op) []string {
	suffix := "_key"
	keyword := "UNIQUE"
	if op.Constraint == statement.ConstraintPrimaryKey {
		suffix = "_pkey"
		keyword = "PRIMARY KEY"
	}
	name := op.Name
	if name == "" {
		name = st.Table + "_" + strings.Join(op.Columns, "_") + suffix
	}
	idx := pgx.Identifier{name}.Sanitize()
	cols := make([]string, len(op.Columns))
	for i, c := range op.Columns {
		cols[i] = pgx.Identifier{c}.Sanitize()
	}
	table := tableIdent(st)
	return []string{
		fmt.Sprintf("CREATE UNIQUE INDEX CONCURRENTLY %s ON %s (%s)", idx, table, strings.Join(cols, ", ")),
		fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s %s USING INDEX %s", table, idx, keyword, idx),
	}
}
