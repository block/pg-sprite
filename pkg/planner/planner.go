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
//
// The rules assume PostgreSQL 14, the oldest major the test matrix runs;
// every rule holds unconditionally across the supported range (14–18).
// Rules that were version-dependent below that floor — fast default
// (PG 11+), SET NOT NULL proven by a validated CHECK (PG 12+), DETACH
// PARTITION CONCURRENTLY (PG 14+) — carry no version annotation because
// the floor makes them unconditional. A rule that varies within the
// supported range must carry an explicit version fact before it lands.
package planner

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// RulesPostgresVersions is the inclusive PostgreSQL major-version range
// the classification rules are derived for (see the package comment and
// docs/postgresql-version-support.md). Offline consumers such as the
// linter stamp it into their reports so a stored result names the
// assumptions behind it.
const RulesPostgresVersions = "14-18"

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

// Routes returns the closed set of Route values, in severity order. It is
// part of the plan-report contract (docs/plan-report.md): the set changes
// only with a format_version bump, and a consumer that meets an
// unrecognized value must treat the statement as unknown and refuse it.
func Routes() []Route {
	return []Route{RouteNative, RouteCopyAndSwap, RouteRefuse}
}

// Reasons returns the closed set of Reason values. It is part of the
// plan-report contract (docs/plan-report.md): the set changes only with a
// format_version bump, and a consumer that meets an unrecognized value must
// treat the decision as unknown and refuse it.
func Reasons() []Reason {
	return []Reason{
		ReasonMetadataOnly,
		ReasonOnlineIdiom,
		ReasonFastDefault,
		ReasonBinaryCoercible,
		ReasonSaferIdiom,
		ReasonVolatileDefault,
		ReasonGeneratedStored,
		ReasonTypeRewrite,
		ReasonRelocation,
		ReasonPartitionParentLock,
		ReasonUnsupportedOperation,
	}
}

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
	// ReasonAppBreakingRename: PostgreSQL executes the rename as a brief
	// metadata-only catalog flip, but it cannot land atomically across
	// running application instances — code still referencing the old
	// column or table name starts erroring the instant it commits. For a
	// column the safe sequence is expand/contract: add the new column,
	// dual-write and backfill, switch reads, then drop the old column as
	// its own reviewed change. For a table, coordinate the rename with
	// the application deploy that adopts the new name.
	ReasonAppBreakingRename Reason = "app-breaking-rename"
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
	// ReasonPartitionParentLock: creating a partition takes a brief ACCESS
	// EXCLUSIVE on the partitioned parent — no scan, but it queues behind
	// and then blocks every query on the parent while held.
	ReasonPartitionParentLock Reason = "partition-parent-lock"
	// ReasonUnsupportedOperation: the planner does not recognize the
	// operation or knows no safe path for it.
	ReasonUnsupportedOperation Reason = "unsupported-operation"
)

// Execution is the typed execution contract for a planner-produced SQL
// sequence; automation branches on it, never on prose. It tells a consumer
// how the steps must run and that a failed step can leave partial state
// the runner owns detecting and recovering.
type Execution string

// The execution contracts a sequence can carry.
const (
	// ExecutionAutocommit: the steps run one at a time, in order, each in
	// its own implicit transaction — never inside an enclosing transaction
	// block. The CONCURRENTLY forms refuse an enclosing block outright,
	// and a multi-step sequence inside one block holds every earlier
	// step's locks across the steps designed to avoid them. A failed step
	// leaves partial state the runner must detect and recover before
	// retrying (a failed CONCURRENTLY build leaves an invalid index,
	// pg_index.indisvalid = false).
	ExecutionAutocommit Execution = "autocommit-each-step"
)

// Executions returns the closed set of Execution values. It is part of the
// plan-report contract (docs/plan-report.md): the set changes only with a
// format_version bump, and a consumer that meets an unrecognized value must
// treat the sequence as unknown and refuse to run it.
func Executions() []Execution {
	return []Execution{ExecutionAutocommit}
}

// Decision is the classification of one operation.
type Decision struct {
	// Operation is the operator-facing label (display only).
	Operation string `json:"operation"`
	// Destructive marks operations that discard live structure — a dropped
	// column, constraint, or index. It is derived from the operation shape
	// here, in the one place every front door shares, so a plan reports the
	// same statement as destructive no matter how it was submitted. It is
	// always emitted, never omitted: a safety flag a consumer gates on must
	// be explicit even when false.
	Destructive bool `json:"destructive"`
	// Route is where the operation goes.
	Route Route `json:"route"`
	// Reason is why.
	Reason Reason `json:"reason"`
	// Unverified marks a decision the planner took without the live facts
	// needed to prove a cheaper one — it failed closed to the heavier
	// route. The route is what the engine would do, not a proven property
	// of the change: with facts (a live introspection or a supplied
	// column type) the same operation may classify as native.
	Unverified bool `json:"unverified,omitempty"`
	// SaferSQL is the ordered safer native sequence, present only for
	// safer-idiom decisions where the planner could construct it. It is a
	// safer form of the submitted statement, not a semantic equivalent: it
	// converges on the same declared end state with different locking,
	// transactionality, and failure modes. SaferSQLExecution carries the
	// execution contract: the steps run one at a time, in order, each in
	// its own implicit transaction — never inside an enclosing transaction
	// block, which the CONCURRENTLY forms refuse. Each sequence constructor
	// documents what a failed step leaves behind and how a retry resumes
	// (a failed CONCURRENTLY build leaves an invalid index the runner must
	// detect via pg_index.indisvalid and rebuild).
	SaferSQL []string `json:"safer_sql,omitempty"`
	// SaferSQLExecution is the typed execution contract for SaferSQL,
	// present exactly when SaferSQL is. Automation branches on it instead
	// of prose — it is what tells a consumer the sequence must not be
	// wrapped in a transaction block.
	SaferSQLExecution Execution `json:"safer_sql_execution,omitempty"`
}

// ExecutableAsSubmitted reports whether the operation's submitted form is
// itself safe to run. It is false exactly for safer-idiom decisions: their
// submitted form blocks and must be replaced by the safer sequence —
// whether or not one was constructed. Routing fails closed on the
// combination of a false ExecutableAsSubmitted and an empty SaferSQL.
func (d Decision) ExecutableAsSubmitted() bool {
	return d.Reason != ReasonSaferIdiom
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

// Facts are properties of the live table that sharpen classification, and
// they are trusted as stated: the CLI fills them by introspecting the
// target database, and a library caller may supply facts it already holds —
// but they must describe the database the change will run on, because a
// wrong fact can upgrade a rewrite to native. Missing facts are always
// safe: the zero value is valid and classifies strictly more
// conservatively (every type change becomes copy-and-swap).
type Facts struct {
	// ColumnTypes maps a column name to its live type as rendered by
	// PostgreSQL's format_type (e.g. "character varying(50)").
	ColumnTypes map[string]string
}

// FactsFrom extracts the facts a live introspection model provides: the
// canonical type of every live column. Every front door — the declarative
// diff and the imperative dry-run — extracts facts through this one
// function so equivalent changes classify identically.
func FactsFrom(live schemadiff.Model) Facts {
	types := make(map[string]string, len(live.Columns))
	for _, col := range live.Columns {
		types[col.Name] = col.Type
	}
	return Facts{ColumnTypes: types}
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
		if len(d.SaferSQL) > 0 {
			// Stamped here, in the one place every decision passes
			// through, so a constructed sequence can never ship without
			// its execution contract.
			d.SaferSQLExecution = ExecutionAutocommit
		}
		plan.Route = worse(plan.Route, d.Route)
		plan.Decisions = append(plan.Decisions, d)
	}
	return plan, nil
}

// classifyOp routes one operation per the reference table.
func classifyOp(op statement.Op, st statement.Statement, facts Facts, sql string, single bool) Decision {
	d := Decision{Operation: op.Describe(), Destructive: destructiveOp(op.Kind)}
	switch op.Kind {
	case statement.OpCreateTable:
		if op.PartitionOf {
			// Creating a partition locks the partitioned parent ACCESS
			// EXCLUSIVE — briefly and without a scan, but it queues behind
			// any long-running query and then blocks every reader of the
			// parent while held.
			d.Route, d.Reason = RouteNative, ReasonPartitionParentLock
		} else {
			// A brand-new standalone table has no readers to lock out.
			d.Route, d.Reason = RouteNative, ReasonMetadataOnly
		}

	case statement.OpAddColumn:
		switch {
		case hasUnrecognizedConstraint(op.InlineConstraints):
			d.Route, d.Reason = RouteRefuse, ReasonUnsupportedOperation
		case op.GeneratedStored:
			d.Route, d.Reason = RouteCopyAndSwap, ReasonGeneratedStored
		case op.Default == statement.DefaultExpression:
			d.Route, d.Reason = RouteCopyAndSwap, ReasonVolatileDefault
		case len(op.InlineConstraints) > 0:
			// An inline UNIQUE / PRIMARY KEY / FOREIGN KEY / CHECK does the
			// same index build or validation as its ADD CONSTRAINT form,
			// under the ADD COLUMN's ACCESS EXCLUSIVE lock. The safer path
			// splits the column addition from an online constraint build;
			// the planner does not construct multi-statement splits, so the
			// decision carries no rewrite and routing fails closed.
			d.Route, d.Reason = RouteNative, ReasonSaferIdiom
		case op.Default == statement.DefaultConstant:
			d.Route, d.Reason = RouteNative, ReasonFastDefault
		default:
			d.Route, d.Reason = RouteNative, ReasonMetadataOnly
		}

	case statement.OpDropColumn, statement.OpSetDefault, statement.OpDropDefault,
		statement.OpDropNotNull,
		statement.OpRenameIndex, statement.OpSetColumnOptions, statement.OpSetRelOptions,
		statement.OpSetSchema, statement.OpDropConstraint:
		d.Route, d.Reason = RouteNative, ReasonMetadataOnly

	case statement.OpRenameColumn, statement.OpRenameTable:
		// Metadata-only for PostgreSQL, but not for the application: a
		// rename cannot land atomically across deployed instances, so
		// code querying the old name breaks the instant it commits. The
		// engine still executes it when asked; the typed reason lets
		// lint and plan consumers steer to a safe sequence instead.
		// Index renames stay metadata-only above — SQL never references
		// an index by name.
		d.Route, d.Reason = RouteNative, ReasonAppBreakingRename

	case statement.OpAlterColumnType:
		d.Route, d.Reason, d.Unverified = classifyTypeChange(op, facts)

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

// destructiveOp reports whether an operation shape discards live
// structure. A drop is destructive regardless of how it routes: a dropped
// column discards data, a dropped constraint discards a guarantee the
// schema was providing, and a dropped index discards a structure that is
// expensive to rebuild (and, for a unique index, the uniqueness guarantee).
func destructiveOp(kind statement.OpKind) bool {
	switch kind {
	case statement.OpDropColumn, statement.OpDropConstraint, statement.OpDropIndex:
		return true
	default:
		return false
	}
}

// concurrentlyDecision routes an operation that is online in its
// CONCURRENTLY form: already concurrent is the idiom; otherwise native with
// the concurrent rewrite as the safer sequence. The rewrite trades the
// blocking lock for a different failure mode — non-transactional, and a
// failed build leaves an invalid index — which the executor, not the
// planner, guards.
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
	case op.IndexName != "", op.NotValid:
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
func classifyTypeChange(op statement.Op, facts Facts) (Route, Reason, bool) {
	if op.HasUsing {
		return RouteCopyAndSwap, ReasonTypeRewrite, false
	}
	oldType, ok := facts.ColumnTypes[op.Column]
	if !ok {
		// No live type for the column: fail closed to the rewrite route,
		// but say so — the conversion may be a free relabel that a fact
		// would prove.
		return RouteCopyAndSwap, ReasonTypeRewrite, true
	}
	if binaryCoercible(parseTypeText(oldType), typeShape{name: normalizeTypeName(op.NewType), mods: op.NewTypeMods}) {
		return RouteNative, ReasonBinaryCoercible, false
	}
	return RouteCopyAndSwap, ReasonTypeRewrite, false
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

// hasUnrecognizedConstraint reports whether an added column carries an
// inline constraint family the engine does not model.
func hasUnrecognizedConstraint(kinds []statement.ConstraintKind) bool {
	return slices.Contains(kinds, statement.ConstraintUnrecognized)
}

// maxIdentifierBytes is PostgreSQL's NAMEDATALEN-1: the server silently
// truncates longer identifiers, which would let a generated name collide
// with the table itself or with a sibling scaffold.
const maxIdentifierBytes = 63

// fitIdentifier returns name unchanged when it fits PostgreSQL's identifier
// limit, otherwise a deterministic variant that does: the head of the name
// plus an 8-hex-digit hash of the full name, so distinct inputs stay
// distinct after fitting.
func fitIdentifier(name string) string {
	if len(name) <= maxIdentifierBytes {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	suffix := fmt.Sprintf("_%x", sum[:4])
	head := name[:maxIdentifierBytes-len(suffix)]
	for !utf8.ValidString(head) {
		head = head[:len(head)-1]
	}
	return head + suffix
}

// tableIdent renders the statement's target table as a quoted identifier,
// schema-qualified when the statement was.
func tableIdent(st statement.Statement) string {
	if st.Schema() != "" {
		return pgx.Identifier{st.Schema(), st.Table()}.Sanitize()
	}
	return pgx.Identifier{st.Table()}.Sanitize()
}

// setNotNullSequence is the native four-step SET NOT NULL pattern: prove
// the invariant online with a NOT VALID CHECK, flip the column, drop the
// scaffold.
//
// Partial-failure contract: step 1 leaves a NOT VALID CHECK under the
// generated scaffold name, and re-running step 1 then fails with SQLSTATE
// 42710 (duplicate_object) — a retry resumes at step 2. A failed VALIDATE
// (step 2) leaves the same scaffold and is safe to re-run. Steps 3 and 4
// are metadata-only and safe to re-run; a leftover scaffold is removed by
// running step 4 alone.
func setNotNullSequence(st statement.Statement, column string) []string {
	table := tableIdent(st)
	conName := fitIdentifier(fmt.Sprintf("%s_%s_not_null", st.Table(), column))
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
//
// Partial-failure contract: a failed CREATE INDEX CONCURRENTLY (step 1)
// leaves an INVALID index under the generated name, and re-running step 1
// then fails with SQLSTATE 42P07 (duplicate_table) — the retry path is
// DROP INDEX, then re-run step 1. Step 2 consumes the index into the
// constraint under a brief lock and does not scan.
func usingIndexSequence(st statement.Statement, op statement.Op) []string {
	suffix := "_key"
	keyword := "UNIQUE"
	if op.Constraint == statement.ConstraintPrimaryKey {
		suffix = "_pkey"
		keyword = "PRIMARY KEY"
	}
	name := op.Name
	if name == "" {
		// A user-supplied name is used as-is: the server truncates it the
		// same way in every step. A generated name is built to fit so it
		// cannot truncate into the table's own name or a sibling's.
		name = fitIdentifier(st.Table() + "_" + strings.Join(op.Columns, "_") + suffix)
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
