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
	"cmp"
	"crypto/sha256"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/preflight"
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
		ReasonAppBreakingRename,
		ReasonVolatileDefault,
		ReasonGeneratedStored,
		ReasonTypeRewrite,
		ReasonRelocation,
		ReasonPartitionParentLock,
		ReasonReplicaIdentity,
		ReasonDependentView,
		ReasonDependentPolicy,
		ReasonDependentTrigger,
		ReasonDependentPublication,
		ReasonDependentGeneratedColumn,
		ReasonPartitionKey,
		ReasonNotNullWithoutDefault,
		ReasonColumnLimit,
		ReasonCascadingDrop,
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
	// ReasonFastDefault: ADD COLUMN with a default the catalog stores, so
	// no row is rewritten (PG 11+): a constant, or any default the server
	// proved does not rewrite (a non-volatile expression such as now()).
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
	// column, table, or constraint name starts erroring the instant it
	// commits. For a column the safe sequence is expand/contract: add the
	// new column, dual-write and backfill, switch reads, then drop the old
	// column as its own reviewed change. For a table, coordinate the
	// rename with the application deploy that adopts the new name. For an
	// index that backs a constraint, the rename renames the constraint,
	// so coordinate it with the deploy that adopts the new constraint
	// name. No sequence can be constructed from the statement, so the
	// rename is rewrite-required.
	ReasonAppBreakingRename Reason = "app-breaking-rename"
	// ReasonVolatileDefault: ADD COLUMN whose default PostgreSQL computes
	// for every row, rewriting the table — proven by the server, or assumed
	// (Unverified) when the default is not a constant and nothing proved
	// it.
	ReasonVolatileDefault Reason = "volatile-default"
	// ReasonGeneratedStored: adding a stored generated column computes
	// every row — a full rewrite.
	ReasonGeneratedStored Reason = "generated-stored"
	// ReasonTypeRewrite: a type PostgreSQL must check or convert row by
	// row — a type conversion it cannot relabel (rewrite plus reindex), a
	// type change that moves the column's collation (reindex alone), or an
	// added column of a domain type with constraints.
	ReasonTypeRewrite Reason = "type-rewrite"
	// ReasonRelocation: SET TABLESPACE moves the heap — a rewrite-scale
	// copy.
	ReasonRelocation Reason = "relocation"
	// ReasonPartitionParentLock: creating a partition takes a brief ACCESS
	// EXCLUSIVE on the partitioned parent — no scan, but it queues behind
	// and then blocks every query on the parent while held.
	ReasonPartitionParentLock Reason = "partition-parent-lock"
	// ReasonReplicaIdentity: the operation drops what the table's replica
	// identity index keys on — the index itself, the constraint that owns
	// it, or one of its columns. The index is the one REPLICA IDENTITY
	// USING INDEX selects, or the primary key under REPLICA IDENTITY
	// DEFAULT while a publication publishes the table's updates or
	// deletes. The setting outlives the index, so the table is left with
	// no usable identity, and every UPDATE and DELETE on it fails while a
	// publication publishes them. Move the identity first (ALTER TABLE ...
	// REPLICA IDENTITY), then re-plan.
	ReasonReplicaIdentity Reason = "replica-identity"
	// ReasonDependentView: the operation changes the type of, or drops, a
	// column that a view or materialized view reads. PostgreSQL refuses
	// both while the view exists, even a type change that rewrites
	// nothing, so the statement could only fail. The decision names the
	// views; change or drop them first, then re-plan.
	ReasonDependentView Reason = "dependent-view"
	// ReasonDependentPolicy: the operation changes the type of, or drops,
	// a column that a row-level security policy reads, in its USING or
	// WITH CHECK expression, on this table or on another table that reads
	// the column through a subquery. PostgreSQL refuses both while the
	// policy exists, even a type change that rewrites nothing, so the
	// statement could only fail. The decision names the policies; drop
	// them first and recreate them after the change, then re-plan.
	ReasonDependentPolicy Reason = "dependent-policy"
	// ReasonDependentTrigger: the operation changes the type of, or drops,
	// a column that a trigger's UPDATE OF list or WHEN condition names.
	// PostgreSQL refuses both while the trigger exists, even a type change
	// that rewrites nothing, so the statement could only fail. The
	// decision names the triggers; a trigger whose function reads the
	// column does not count, since the server records no dependency for
	// it.
	ReasonDependentTrigger Reason = "dependent-trigger"
	// ReasonDependentPublication: the operation changes the type of, or
	// drops, a column that a publication's column list or row filter for
	// the table names. PostgreSQL refuses both while the publication
	// names it, so the statement could only fail, and a desired-state run
	// would stop part-way with the steps before it committed. The decision
	// names the publications; a publication that publishes the table
	// without a column list or row filter does not count.
	ReasonDependentPublication Reason = "dependent-publication"
	// ReasonDependentGeneratedColumn: the operation changes the type of,
	// or drops, a column that a generated column's expression reads.
	// PostgreSQL refuses both while the generated column exists, so the
	// statement could only fail, except that PostgreSQL 14 drops the
	// generated column along with a dropped column, so there the refusal
	// is what keeps it. The decision names the generated columns.
	ReasonDependentGeneratedColumn Reason = "dependent-generated-column"
	// ReasonPartitionKey: the operation changes the type of, or drops, a
	// column in the partition key of the table or of a partitioned table
	// below it that the change reaches. PostgreSQL never changes or drops
	// a partition key column, so the statement could only fail; the only
	// path is a new partitioned table with the new key, the rows moved
	// into it. The decision names the partitioned tables.
	ReasonPartitionKey Reason = "partition-key"
	// ReasonNotNullWithoutDefault: the operation adds a NOT NULL column
	// with no default to a table that holds rows. Every existing row would
	// hold NULL in the new column, so PostgreSQL refuses the statement
	// (SQLSTATE 23502) after taking the table's exclusive lock; it could
	// only fail. A default, identity, or generated column fills the rows,
	// and an empty table has none to fill, so neither is refused.
	ReasonNotNullWithoutDefault Reason = "not-null-without-default"
	// ReasonColumnLimit: the operation adds a column to a table that has
	// used every column number PostgreSQL gives a table, or that a table
	// inheriting from it has. A dropped column keeps its number until the
	// table is rebuilt, so PostgreSQL refuses the statement (SQLSTATE
	// 54011) however few live columns the table has; it could only fail.
	ReasonColumnLimit Reason = "column-limit"
	// ReasonCascadingDrop: a drop with CASCADE, which also drops whatever
	// depends on what it drops: a view or policy that reads a dropped
	// column, or another table's foreign key on a dropped key. Those
	// objects are not in the statement, so neither the plan nor the
	// table's lock covers them. Drop each dependent object as its own
	// change, then resubmit without CASCADE, which PostgreSQL refuses
	// while anything still depends on the target.
	ReasonCascadingDrop Reason = "cascading-drop"
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
	// ExecutionPartitionedIndexBuild: the one step is a CREATE INDEX on a
	// partitioned table, and it runs as the partition-aware index build, not
	// as the statement: the parent's index is created with ONLY, each
	// partition's index is built concurrently and attached, and the parent's
	// index turns valid with the last attach. The partitions are listed when
	// the build runs, not when the plan is made. The statement must not be
	// run as written, which would build every partition's index under a lock
	// that blocks writes to the whole tree. A failed or interrupted run
	// leaves an invalid parent index and the partition indexes built so far;
	// running the same statement again resumes from them.
	ExecutionPartitionedIndexBuild Execution = "partitioned-index-build"
	// ExecutionPartitionedForeignKeyBuild: the one step is an ALTER TABLE
	// ... ADD FOREIGN KEY on a partitioned table, on a server before
	// PostgreSQL 18, and it runs as the partitioned foreign key build, not as
	// the statement: each partition is given the foreign key NOT VALID and
	// then validated while writes continue, and the parent's ADD FOREIGN KEY
	// then adopts every partition's in one brief transaction that validates
	// nothing. The partitions are listed when the build runs, not when the
	// plan is made. The statement must not be run as written, which would
	// validate every partition while it blocks writes to the whole tree. A
	// failed or interrupted run leaves the partition foreign keys finished
	// so far; running the same statement again resumes from them.
	ExecutionPartitionedForeignKeyBuild Execution = "partitioned-foreign-key-build"
	// ExecutionBlockingIndexDrop: the one step is the plain DROP INDEX of a
	// partitioned table's index, which PostgreSQL cannot drop concurrently.
	// It takes ACCESS EXCLUSIVE on the parent, every partition, and every
	// partition's index for as long as it runs, so the statement stays
	// refused and runs only when the operator accepts that lock, in one
	// engine-owned transaction under explicit lock and statement budgets.
	ExecutionBlockingIndexDrop Execution = "blocking-index-drop"
	// ExecutionPartitionedKeyBuild: the one step is an ALTER TABLE ... ADD
	// PRIMARY KEY or ADD UNIQUE on a partitioned table, and it runs as the
	// partitioned key build, not as the statement: each partition's unique
	// index is built concurrently and given the constraint with ADD
	// CONSTRAINT ... USING INDEX, then the parent's ADD CONSTRAINT adopts
	// them in one brief transaction that builds nothing. The partitions are
	// listed when the build runs, not when the plan is made. The statement
	// must not be run as written, which would build every partition's index
	// under ACCESS EXCLUSIVE on the whole tree. A failed or interrupted run
	// leaves the partition constraints finished so far; running the same
	// statement again resumes from them.
	ExecutionPartitionedKeyBuild Execution = "partitioned-key-build"
)

// Executions returns the closed set of Execution values. It is part of the
// plan-report contract (docs/plan-report.md): the set changes only with a
// format_version bump, and a consumer that meets an unrecognized value must
// treat the sequence as unknown and refuse to run it.
func Executions() []Execution {
	return []Execution{ExecutionAutocommit, ExecutionPartitionedIndexBuild, ExecutionBlockingIndexDrop, ExecutionPartitionedKeyBuild, ExecutionPartitionedForeignKeyBuild}
}

// Decision is the classification of one operation.
type Decision struct {
	// Operation is the operator-facing label (display only).
	Operation string `json:"operation"`
	// Kind is the operation's parsed shape. The router's availability
	// policy branches on it together with Reason: whether a backend
	// carries a rewrite depends on what the operation does to the table,
	// not only on why PostgreSQL would rewrite it. It is not part of the
	// plan-report contract; a consumer branches on Reason.
	Kind statement.OpKind `json:"-"`
	// Destructive marks operations that discard live structure — a dropped
	// column, constraint, index, or NOT NULL. It is derived from the operation shape
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
	// ProvenCatalogOnly marks an operation PostgreSQL runs on the catalog
	// alone, never rewriting or reading the table's rows, so its cost does
	// not grow with the table. Unlike Reason metadata-only, which is the
	// planner's classification, it is set only where that is certain: for
	// DROP COLUMN, DROP DEFAULT, DROP NOT NULL, and DROP CONSTRAINT, which
	// PostgreSQL defines that way, and for an ADD COLUMN the server's probe
	// proved adds in place without a scan. SET DEFAULT is not marked: the
	// server coerces the new default to the column's type under the lock,
	// and for a user-defined type that runs the type's input function. Absent means
	// not established, not that the operation reads rows. An event trigger
	// is outside the proof: its code runs under the statement's lock
	// whatever the table's size, bounded by the attempt's budget like any
	// other statement.
	ProvenCatalogOnly bool `json:"proven_catalog_only,omitempty"`
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
	// DependentViews names the views and materialized views that read the
	// column, each schema-qualified with every identifier quoted, present
	// whenever one blocks the change, whatever the decision's reason.
	DependentViews []string `json:"dependent_views,omitempty"`
	// DependentMaterializedViews names the materialized views among
	// DependentViews and DependentViewDropOrder, rendered the same way, so
	// the help can call each by its kind; recreating one runs its query to
	// fill it again. Like DependentFix, it is not part of the plan-report
	// contract.
	DependentMaterializedViews []string `json:"-"`
	// DependentViewDropOrder names every view the manual path drops to
	// change DependentViews: those views and the views that read them,
	// directly or through other views, deepest first, so each comes before
	// every view it reads. PostgreSQL refuses to drop a view another view
	// reads, so they are dropped in this order and recreated in reverse.
	// It is nil when no view reads another, where any order works. Like
	// DependentFix, it is not part of the plan-report contract.
	DependentViewDropOrder []string `json:"-"`
	// DependentPolicies names the row-level security policies that read
	// the column, each rendered "policy" ON "schema"."table" with every
	// identifier quoted, present whenever one blocks the change, whatever
	// the decision's reason.
	DependentPolicies []string `json:"dependent_policies,omitempty"`
	// DependentTriggers names the triggers whose UPDATE OF list or WHEN
	// condition names the column, each rendered "trigger" ON
	// "schema"."table" with every identifier quoted, present whenever one
	// blocks the change, whatever the decision's reason.
	DependentTriggers []string `json:"dependent_triggers,omitempty"`
	// DependentPublications names the publications whose column list or
	// row filter for the table names the column, each rendered
	// "publication" with its identifier quoted, present whenever one
	// blocks the change, whatever the decision's reason.
	DependentPublications []string `json:"dependent_publications,omitempty"`
	// DependentGeneratedColumns names the generated columns whose
	// expression reads the column, each rendered "schema"."table"."column"
	// with every identifier quoted, present whenever one blocks the change,
	// whatever the decision's reason. A copy that dropping the table's own
	// generated column removes is left out, since PostgreSQL refuses to
	// drop it on its own.
	DependentGeneratedColumns []string `json:"dependent_generated_columns,omitempty"`
	// PartitionKeyTables names the partitioned tables whose partition key
	// includes the column, each schema-qualified with every identifier
	// quoted, present exactly for partition-key decisions.
	PartitionKeyTables []string `json:"partition_key_tables,omitempty"`
	// ColumnLimit is how many column numbers the table had left before the
	// change, and which of it and its partitions or inheritance children
	// have used the most, present exactly for column-limit decisions.
	ColumnLimit *preflight.ColumnNumbers `json:"column_limit,omitempty"`
	// NotNullFromType is the added column's type, as the statement spells
	// it with every identifier quoted, when that type is what makes the column NOT NULL: a domain that
	// declares NOT NULL, on a column that spells neither NOT NULL nor
	// PRIMARY KEY. Present only for not-null-without-default decisions.
	// Adding a nullable column of that type is impossible, so the manual
	// path is the type's base type instead (see NotNullFromTypeHelp).
	NotNullFromType string `json:"not_null_from_type,omitempty"`
	// ConstrainedType is the added column's type, as the statement spells
	// it with every identifier quoted, when that type is a domain with constraints, which PostgreSQL
	// checks against every row by rewriting the table whatever default the
	// column is given. Present only for type-rewrite decisions. The manual
	// path is the domain's base type with its constraints on the column
	// instead (see ConstrainedTypeHelp).
	ConstrainedType string `json:"constrained_type,omitempty"`
	// DependentFix is the manual path for the views, policies, and
	// triggers that block the change, which depends on what the change
	// would do with those dependents out of the way. Like Kind, it is not part
	// of the plan-report contract: it selects the help the engine's own
	// text renders.
	DependentFix DependentFix `json:"-"`
	// ReadsInTimeZone names the session time zone a change between
	// timestamp and timestamptz reads every stored value in, so the
	// engine's text can say which instant each value becomes. Empty for
	// any other operation, or when the zone was not read. Like Kind, it is
	// not part of the plan-report contract.
	ReadsInTimeZone string `json:"-"`
}

// DependentFix is the manual path for a change PostgreSQL refuses while a
// view, a row-level security policy, or a trigger reads the column:
// objects that can be dropped and recreated around the change.
type DependentFix string

const (
	// DependentFixRewriteDependents is for a column drop. The dependents
	// have to stop reading the column before it can go, and the drop then
	// plans on its own.
	DependentFixRewriteDependents DependentFix = "rewrite-dependents"
	// DependentFixRecreateAroundChange is for a type change PostgreSQL runs
	// in place. Dropping the dependents, running the change, and
	// recreating them in one transaction holds the table's exclusive lock
	// only briefly, and no session sees the table without them; a
	// materialized view holds it while its query fills it again.
	DependentFixRecreateAroundChange DependentFix = "recreate-around-change"
	// DependentFixExpandContract is for a type change that rewrites the
	// table. Recreating the dependents around it would hold the exclusive
	// lock for the whole rewrite, and copy-and-swap rebuilds the table's
	// policies before it retypes the column, so it meets the same refusal
	// (it refuses a table with triggers outright). A new column of the
	// target type takes the dependents over instead.
	DependentFixExpandContract DependentFix = "expand-contract"
)

// GeneratedColumnHelp is the operator-facing manual path for a
// dependent-generated-column decision, phrased to follow a sentence that
// names the generated columns.
const GeneratedColumnHelp = "drop those generated columns first, then re-plan; " +
	"adding a stored one back afterwards, or changing a stored one's expression instead, rewrites the table"

// PublicationHelp is the operator-facing manual path for a
// dependent-publication decision on an operation of kind, phrased to
// follow a sentence that names the publications.
func PublicationHelp(kind statement.OpKind) string {
	if kind == statement.OpDropColumn {
		return "remove the column from those publications' column lists and row filters with ALTER PUBLICATION ... SET TABLE, then re-plan the drop"
	}
	return "remove the column from those publications' column lists and row filters with ALTER PUBLICATION ... SET TABLE, run the change, then add it back; " +
		"until it is back, subscribers do not receive the column, or receive rows the row filter would have held back"
}

// PartitionKeyHelp is the operator-facing manual path for a partition-key
// decision, phrased to follow a sentence that names the partitioned
// tables.
const PartitionKeyHelp = "create a new partitioned table with the new column definition and move the rows into it"

// DependentsPhrase names dependents after "read by": the singular noun
// before one name and the plural before several, joined with commas.
func DependentsPhrase(singular, plural string, names []string) string {
	noun := singular
	if len(names) > 1 {
		noun = plural
	}
	return noun + " " + strings.Join(names, ", ")
}

// Help is the operator-facing manual path, phrased to follow a sentence
// that names the dependents; plural is their noun, such as "policies".
func (f DependentFix) Help(plural string) string {
	switch f {
	case DependentFixRewriteDependents:
		return "change those " + plural + " so they no longer read the column, then re-plan the drop"
	case DependentFixRecreateAroundChange:
		return "this type change runs in place, without rewriting the table, so in one transaction drop those " + plural + ", run the change, and recreate them"
	case DependentFixExpandContract:
		return "this type change rewrites the table, so recreating the " + plural + " around it would block the table for the whole rewrite; " +
			"add a column of the new type instead, have the application write to both and backfill it, point those " + plural +
			" at it and switch reads to it, then drop the old column once nothing uses it"
	}
	return string(f)
}

// ViewsPhrase names the views that block the change after "read by", each
// by its kind: view "a"."v", materialized view "a"."m", or views "a"."v",
// "a"."w" and materialized view "a"."m". It is "" when no view blocks it.
func (d Decision) ViewsPhrase() string {
	var plain, materialized []string
	for _, name := range d.DependentViews {
		if slices.Contains(d.DependentMaterializedViews, name) {
			materialized = append(materialized, name)
		} else {
			plain = append(plain, name)
		}
	}
	var parts []string
	if len(plain) > 0 {
		parts = append(parts, DependentsPhrase("view", "views", plain))
	}
	if len(materialized) > 0 {
		parts = append(parts, DependentsPhrase("materialized view", "materialized views", materialized))
	}
	return strings.Join(parts, " and ")
}

// ViewsNoun is the plural noun for the views that block the change, for
// DependentFix.Help: "views", "materialized views", or "views and
// materialized views".
func (d Decision) ViewsNoun() string {
	materialized := 0
	for _, name := range d.DependentViews {
		if slices.Contains(d.DependentMaterializedViews, name) {
			materialized++
		}
	}
	switch materialized {
	case 0:
		return "views"
	case len(d.DependentViews):
		return "materialized views"
	}
	return "views and materialized views"
}

// ViewsHelp is the manual path for the views that block the change,
// phrased to follow a sentence that names them: the dependent fix every
// recreatable dependent shares, or, on a decision that carries none,
// redefining the views before planning again. PostgreSQL changes a view's
// columns only by dropping and recreating it, so the help says when that
// happens, gives the order to drop the views in when one reads another,
// and names what a dropped view takes with it so the reader recreates it
// too.
func (d Decision) ViewsHelp() string {
	noun := d.ViewsNoun()
	help := "drop or redefine those " + noun + " first, then re-plan"
	if d.DependentFix != "" {
		help = d.DependentFix.Help(noun)
	}
	if d.DependentFix == DependentFixRecreateAroundChange && len(d.DependentMaterializedViews) > 0 {
		help += "; recreating a materialized view runs its query to fill it again while the transaction blocks the table"
	}
	if swap := d.viewSwap(); swap != "" {
		help += "; " + swap
	}
	help += "; a dropped view loses its grants, comment, owner, options such as security_barrier, rules, INSTEAD OF triggers and column defaults"
	if len(d.DependentMaterializedViews) > 0 {
		help += ", and a materialized view its indexes too"
	}
	return help + ", so recreate those with each view"
}

// viewSwap says how the views change: by dropping and recreating them, in
// one transaction at the step that changes them, and in the order
// DependentViewDropOrder gives when one view reads another. A fix that
// already drops and recreates the views around the change needs only the
// order.
func (d Decision) viewSwap() string {
	const inPlace = "since CREATE OR REPLACE VIEW cannot remove or retype a column"
	var parts []string
	switch d.DependentFix {
	case DependentFixRecreateAroundChange:
	case DependentFixExpandContract:
		parts = append(parts, "pointing a view at the new column means dropping and recreating it, "+inPlace+", so do that in one transaction at that step")
	default:
		parts = append(parts, "changing a view means dropping and recreating it in one transaction, "+inPlace)
	}
	if len(d.DependentViewDropOrder) > 0 {
		parts = append(parts, "drop the views in this order: "+d.viewDropOrderPhrase()+", and recreate them in reverse")
	}
	return strings.Join(parts, "; ")
}

// viewDropOrderPhrase names DependentViewDropOrder in order, each by its
// kind: view "a"."top", then materialized view "a"."sum".
func (d Decision) viewDropOrderPhrase() string {
	names := make([]string, len(d.DependentViewDropOrder))
	for i, name := range d.DependentViewDropOrder {
		names[i] = "view " + name
		if slices.Contains(d.DependentMaterializedViews, name) {
			names[i] = "materialized view " + name
		}
	}
	return strings.Join(names, ", then ")
}

// viewDropOrder orders views and the views that read them, directly or
// through other views, deepest first and then by name, or returns nil when
// no view reads another. Each view's depth is its longest chain from any
// of views, and a view in views that nothing among them reaches has depth
// zero, so a view always sorts ahead of every view it reads, including
// when one view in views reads another.
func viewDropOrder(views []string, readers map[string]map[string]int) []string {
	depths := map[string]int{}
	for _, view := range views {
		depths[view] = 0
	}
	ordered := false
	for _, view := range views {
		for reader, depth := range readers[view] {
			depths[reader] = max(depths[reader], depth)
			ordered = true
		}
	}
	if !ordered {
		return nil
	}
	return slices.SortedFunc(maps.Keys(depths), func(a, b string) int {
		return cmp.Or(cmp.Compare(depths[b], depths[a]), cmp.Compare(a, b))
	})
}

// OtherDependents phrases the dependents that block the change besides
// the kind its reason names, such as the triggers on a dependent-policy
// decision, or returns "" when there are none. PostgreSQL refuses the
// change until every one of them is out of the way.
func (d Decision) OtherDependents() string {
	var others []string
	add := func(reason Reason, singular, plural string, names []string) {
		if d.Reason != reason && len(names) > 0 {
			others = append(others, DependentsPhrase(singular, plural, names))
		}
	}
	if d.Reason != ReasonDependentView && len(d.DependentViews) > 0 {
		others = append(others, d.ViewsPhrase())
	}
	add(ReasonDependentPolicy, "policy", "policies", d.DependentPolicies)
	add(ReasonDependentTrigger, "trigger", "triggers", d.DependentTriggers)
	add(ReasonDependentPublication, "publication", "publications", d.DependentPublications)
	add(ReasonDependentGeneratedColumn, "generated column", "generated columns", d.DependentGeneratedColumns)
	return strings.Join(others, " and ")
}

// ExecutableAsSubmitted reports whether the operation's submitted form is
// itself safe to run. It is false for the decisions whose submitted form
// must be replaced by a safer sequence, whether or not one was
// constructed: a safer-idiom decision, whose submitted form blocks the
// table, and an app-breaking rename, whose submitted form breaks every
// running application instance that still uses the old name. Routing
// fails closed on the combination of a false ExecutableAsSubmitted and an
// empty SaferSQL.
func (d Decision) ExecutableAsSubmitted() bool {
	switch d.Reason {
	case ReasonSaferIdiom, ReasonAppBreakingRename:
		return false
	default:
		return true
	}
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
// wrong fact can upgrade a rewrite to native. Missing facts never let an
// unsafe change succeed: the zero value is valid, routes every type change
// to copy-and-swap, and only defers some refusals, such as a column a view
// or a policy reads, to the server's own refusal at execution.
type Facts struct {
	// ColumnTypes maps a column name to its live type as rendered by
	// PostgreSQL's format_type (e.g. "character varying(50)").
	ColumnTypes map[string]string
	// ColumnCollations maps each live column whose collation is not its
	// type's default to that collation, as schemadiff renders it. A column
	// absent from the map has the default collation. Retyping a column
	// without a COLLATE clause resets it to the new type's default, so a
	// column present here loses its collation and every index on it is
	// rebuilt: never a relabel, unless the clause names this collation.
	ColumnCollations map[string]string
	// ReplicaIdentity is what the replica identity indexes of the table and
	// the tables below it key on; the zero value when none has one to
	// protect.
	ReplicaIdentity schemadiff.ReplicaIdentityKeys
	// AddColumnProofs maps a column the change adds to what the server
	// proved about adding it (preflight.AddColumnRewrites). A column absent
	// from the map is unproven and classifies by the statement's shape
	// alone, which assumes the worst of any default it cannot read as a
	// constant; the proof leaves out only an ADD COLUMN IF NOT EXISTS for a
	// column the table already has, which the server skips.
	AddColumnProofs map[string]statement.AddColumnProof
	// DependentViews maps a column to the views and materialized views that
	// read it, on the table or on any table below it, which is what a
	// recursive ALTER COLUMN TYPE changes
	// (preflight.ReadColumnDependents). A column absent from the map has
	// none, or the facts were not read, as in an offline lint; either way
	// the server's own refusal at execution remains the backstop.
	DependentViews map[string][]string
	// DropDependentViews is DependentViews for the tables whose copy of
	// the column a recursive DROP COLUMN drops: a child that declares the
	// column itself or also inherits it from a table outside the drop
	// keeps its copy.
	DropDependentViews map[string][]string
	// OwnDependentViews is DependentViews for the table alone, which is
	// all an ALTER TABLE ONLY changes.
	OwnDependentViews map[string][]string
	// MaterializedViews names the materialized views among the dependent
	// views and their readers, rendered as they are
	// (preflight.ColumnDependents).
	MaterializedViews []string
	// ViewReaders maps each dependent view to the views that read it,
	// directly or through other views, each to its longest chain of views
	// from the dependent view (preflight.ColumnDependents).
	ViewReaders map[string]map[string]int
	// DependentPolicies maps a column to the row-level security policies
	// that read it, wherever they are defined, on the table or on any
	// table below it (preflight.ReadColumnDependents), each rendered
	// "policy" ON "schema"."table". A column absent from the map has none,
	// or the facts were not read; either way the server's own refusal at
	// execution remains the backstop.
	DependentPolicies map[string][]string
	// DropDependentPolicies is DependentPolicies for the tables whose copy
	// of the column a recursive DROP COLUMN drops.
	DropDependentPolicies map[string][]string
	// OwnDependentPolicies is DependentPolicies for the table alone.
	OwnDependentPolicies map[string][]string
	// DependentTriggers maps a column to the triggers whose UPDATE OF list
	// or WHEN condition names it (preflight.ReadColumnDependents), each
	// rendered "trigger" ON "schema"."table". A trigger whose function
	// reads the column is not one: the server records no dependency for
	// it and lets the change run.
	DependentTriggers ColumnDependents
	// DependentPublications maps a column to the publications whose column
	// list or row filter for the table names it
	// (preflight.ReadColumnDependents), each rendered "publication".
	DependentPublications ColumnDependents
	// GeneratedColumnCopies maps each generated column to its own copies,
	// named as DependentGeneratedColumns names them, so a statement that
	// drops a generated column stops it blocking a change to the columns
	// it reads (preflight.ReadColumnDependents).
	GeneratedColumnCopies ColumnDependents
	// DependentGeneratedColumns maps a column to the generated columns
	// whose expression reads it, each rendered "schema"."table"."column".
	DependentGeneratedColumns ColumnDependents
	// PartitionKeys maps a column to the partitioned tables whose
	// partition key includes it, each schema-qualified.
	PartitionKeys ColumnDependents
	// NullFilledColumns are the columns the statement adds NOT NULL, as
	// spelled or through the column's domain or an inline PRIMARY KEY,
	// that PostgreSQL would refuse to add because the table, or a
	// partition or inheritance child below it with storage of its own,
	// holds a row the column would be NULL in
	// (preflight.NullFilledColumns). It is read only for the columns
	// NullFillCandidates names. A column absent from the map gets a value
	// in every row, the table holds none, or the fact was not read, as in
	// an offline lint; without it the server's own refusal at execution
	// remains the backstop.
	NullFilledColumns map[string]bool
	// ColumnLimitReached are the columns the statement adds once the table,
	// or a table that inherits from it, has no column number left
	// (ColumnsPastLimit over preflight.ColumnNumbersLeft).
	ColumnLimitReached map[string]bool
	// ColumnNumbers is the read ColumnLimitReached was judged from, which
	// names the tables at the limit for the refusal's help.
	ColumnNumbers preflight.ColumnNumbers
	// ConstraintNames are the names of the constraints already in the
	// table's schema, which the server passes over when it names an
	// unnamed CHECK or FOREIGN KEY. Without them the safer sequence states
	// the server's first choice, and if the table already uses that name
	// its NOT VALID step fails before anything is added.
	ConstraintNames map[string]bool
	// RenamedIndexConstraint is the name of the primary-key, unique, or
	// exclusion constraint the index an ALTER INDEX ... RENAME TO renames
	// backs, which the rename renames with it; empty when the index backs
	// none, or when the fact was not read, as in an offline lint.
	RenamedIndexConstraint string
	// NotNullHelpers are the columns whose SET NOT NULL helper is already on
	// the live table: a CHECK (column IS NOT NULL) under the name
	// NotNullHelperName gives it, left by an earlier run of the online
	// sequence that stopped before its last step dropped it. The sequence
	// resumes at the helper's validation instead of adding it again. A
	// column absent from the map has no such helper, or the fact was not
	// read, as in an offline lint.
	NotNullHelpers map[string]bool
	// Storage reports whether the table has storage of its own: a heap for
	// an ordinary table or a partition, none for a partitioned parent.
	// StorageUnread when the fact was not read, as in an offline lint.
	Storage TableStorage
	// SessionTimeZone is the TimeZone setting the engine's sessions run
	// with (preflight.SessionTimeZone): the zone a change between
	// timestamp and timestamptz reads every stored value in. Empty when
	// the fact was not read, as in an offline lint.
	SessionTimeZone string
	// KeyedColumns are the columns an index or a foreign key reads, on
	// the table or on a table below it (preflight.KeyedColumns). Nil when
	// the fact was not read, as in an offline lint; a column absent from a
	// non-nil map has no index and no foreign key.
	KeyedColumns map[string]bool
}

// TableStorage is what a live read says about a table's own storage.
type TableStorage uint8

const (
	// StorageUnread means the fact was not read, as in an offline lint.
	StorageUnread TableStorage = iota
	// StorageHeap is an ordinary table or a partition, whose rows live in
	// a heap of its own.
	StorageHeap
	// StoragePartitionedParent is a partitioned parent, which has no
	// storage of its own; its rows live in its partitions.
	StoragePartitionedParent
)

// FactsFrom extracts the facts a live introspection model provides: the
// canonical type of every live column, the collation of every column that
// has one of its own, and what the table's replica identity keys on. Every
// front door — the declarative diff and the imperative dry-run — extracts
// facts through this one function so equivalent changes classify
// identically.
func FactsFrom(live schemadiff.Model) Facts {
	types := make(map[string]string, len(live.Columns))
	collations := make(map[string]string)
	for _, col := range live.Columns {
		types[col.Name] = col.Type
		if col.Collation != "" {
			collations[col.Name] = col.Collation
		}
	}
	storage := StorageHeap
	if live.PartitionKey != "" {
		storage = StoragePartitionedParent
	}
	return Facts{ColumnTypes: types, ColumnCollations: collations, ReplicaIdentity: live.ReplicaIdentity,
		Storage: storage}
}

// ColumnDependents maps a column to the objects that depend on it, read
// for each set of tables an ALTER COLUMN TYPE or DROP COLUMN reaches
// (preflight.ReadColumnDependents). A column absent from a map has none,
// or the facts were not read, as in an offline lint; either way the
// server's own refusal at execution remains the backstop.
type ColumnDependents struct {
	// Tree covers the table and every table below it, which is what a
	// recursive ALTER COLUMN TYPE changes.
	Tree map[string][]string
	// Drop covers the tables whose copy of the column a recursive DROP
	// COLUMN drops: a child that declares the column itself or also
	// inherits it from a table outside the drop keeps its copy.
	Drop map[string][]string
	// Own covers the table alone, which is all an ALTER TABLE ONLY
	// changes.
	Own map[string][]string
}

// reached returns the dependents of the column op retypes or drops
// (dependentsReached).
func (c ColumnDependents) reached(op statement.Op) []string {
	return dependentsReached(op, c.Tree, c.Drop, c.Own)
}

// viewsReadingColumn returns the views that read the column op retypes or
// drops (dependentsReached).
func viewsReadingColumn(op statement.Op, facts Facts) []string {
	return dependentsReached(op, facts.DependentViews, facts.DropDependentViews, facts.OwnDependentViews)
}

// policiesReadingColumn returns the policies that read the column op
// retypes or drops (dependentsReached).
func policiesReadingColumn(op statement.Op, facts Facts) []string {
	return dependentsReached(op, facts.DependentPolicies, facts.DropDependentPolicies, facts.OwnDependentPolicies)
}

// dependentsReached returns the dependents that read the column op
// retypes or drops: the two column changes PostgreSQL refuses while a
// view, a policy, a trigger, a publication, a generated column, or a
// partition key reads the column. A rename is not one of them, since each follows the column. Under ONLY the change stops at
// the table, so only its own dependents count, and a recursive drop
// counts only the copies of the column it drops.
func dependentsReached(op statement.Op, tree, drop, own map[string][]string) []string {
	switch {
	case op.Kind != statement.OpAlterColumnType && op.Kind != statement.OpDropColumn:
		return nil
	case op.Only:
		return own[op.Column]
	case op.Kind == statement.OpDropColumn:
		return drop[op.Column]
	default:
		return tree[op.Column]
	}
}

// dependentFix picks the manual path for a change that dependents block:
// a drop needs them rewritten, and a type change is judged by what the
// conversion itself costs, whatever else also blocks it.
func dependentFix(op statement.Op, facts Facts) DependentFix {
	if op.Kind == statement.OpDropColumn {
		return DependentFixRewriteDependents
	}
	// The operator runs the manual path in a session of their own, whose
	// time zone the plan did not read, so a change between timestamp and
	// timestamptz is not promised to stay in place there.
	if swapsTimeZone(op, facts) {
		return DependentFixExpandContract
	}
	if route, _, _ := classifyTypeChange(op, facts); route == RouteNative {
		return DependentFixRecreateAroundChange
	}
	return DependentFixExpandContract
}

// NotNullWithoutDefaultHelp is the operator-facing manual path for a
// not-null-without-default decision.
const NotNullWithoutDefaultHelp = "give the column a constant DEFAULT, which PostgreSQL stores without rewriting the table, " +
	"or add it without NOT NULL, backfill it, then set NOT NULL"

// ColumnLimitSubject names what is at the column limit for a column-limit
// decision: the table itself, its partitions or inheritance children that have
// used the most numbers, or both. When numbers are left it says how many, so
// a refusal of the adds past them agrees with the help's offer to add fewer.
func ColumnLimitSubject(numbers *preflight.ColumnNumbers) string {
	full := fmt.Sprintf("all %d of PostgreSQL's column numbers", preflight.MaxColumnNumber)
	if numbers != nil && numbers.Left > 0 {
		full = fmt.Sprintf("all but %d of PostgreSQL's %d column numbers", numbers.Left, preflight.MaxColumnNumber)
	}
	root, children := columnLimitTables(numbers)
	switch {
	case len(children) == 0:
		return "a table that has used " + full
	case root:
		return fmt.Sprintf("a table that has used %s, as have its %s", full, childrenPhrase(children))
	case len(children) == 1:
		return fmt.Sprintf("a table whose %s has used %s", childrenPhrase(children), full)
	default:
		return fmt.Sprintf("a table whose %s have used %s", childrenPhrase(children), full)
	}
}

// ColumnLimitHelp is the operator-facing manual path for a column-limit
// decision: rebuild the tables at the limit, since copying a table gives back
// the numbers its dropped columns hold, and add fewer columns only when the
// table has a number left for some. Without the read it names the table alone.
func ColumnLimitHelp(numbers *preflight.ColumnNumbers) string {
	const givesBack = "which gives back the numbers its dropped columns hold (VACUUM FULL does not)"
	root, children := columnLimitTables(numbers)
	var rebuild string
	switch {
	case len(children) == 0:
		rebuild = "copy the table into a new one, " + givesBack
	case root:
		rebuild = "copy the table into a new one, " + givesBack + ", and copy its " + childrenPhrase(children) + " the same way"
	case len(children) == 1 && children[0].Partition:
		rebuild = "copy partition " + children[0].Name + " into a new table, " + givesBack +
			", then detach the partition and attach the copy in its place"
	case len(children) == 1:
		rebuild = "copy inheritance child " + children[0].Name + " into a new table, " + givesBack +
			", then make the copy inherit from the same parent in its place"
	default:
		rebuild = "copy each of the " + childrenPhrase(children) + " into a new table, " + givesBack +
			", then put each copy in its table's place: detach a partition and attach its copy, or make a child's copy inherit from the same parent"
	}
	switch {
	case numbers == nil:
		return rebuild
	case numbers.Left == 0:
		return rebuild + "; until then no column number is free"
	case numbers.Left == 1:
		return rebuild + ", or add at most 1 column"
	default:
		return fmt.Sprintf("%s, or add at most %d columns", rebuild, numbers.Left)
	}
}

// columnLimitTables splits the tables at the column limit into whether the
// table the statement alters is one of them and its partitions or inheritance
// children that are.
func columnLimitTables(numbers *preflight.ColumnNumbers) (root bool, children []preflight.FullTable) {
	if numbers == nil {
		return false, nil
	}
	for _, t := range numbers.Fullest {
		if t.Root {
			root = true
			continue
		}
		children = append(children, t)
	}
	return root, children
}

// childrenPhrase names partitions or inheritance children with the noun that
// fits them: "partition "s"."p1"", "partitions "s"."p1", "s"."p2"", or
// "inheritance children ..." once any of them is not a partition.
func childrenPhrase(children []preflight.FullTable) string {
	names := make([]string, 0, len(children))
	partitions := true
	for _, t := range children {
		names = append(names, t.Name)
		partitions = partitions && t.Partition
	}
	noun := map[bool]string{true: "partition", false: "inheritance child"}[partitions]
	if len(children) > 1 {
		noun = map[bool]string{true: "partitions", false: "inheritance children"}[partitions]
	}
	return noun + " " + strings.Join(names, ", ")
}

// ColumnsPastLimit names the columns ops add once no column number is left,
// given the numbers left before ops run, and returns how many are left after
// them. columns are the table's live columns: adding one it already has
// takes no number, unless an earlier operation of ops drops it. A dropped
// column does not give its number back.
func ColumnsPastLimit(ops []statement.Op, columns map[string]string, left int) (map[string]bool, int) {
	dropped := map[string]bool{}
	var past map[string]bool
	for _, op := range ops {
		switch op.Kind {
		case statement.OpDropColumn:
			dropped[op.Column] = true
			continue
		case statement.OpAddColumn:
		default:
			continue
		}
		if _, exists := columns[op.Column]; exists && !dropped[op.Column] {
			continue
		}
		if left > 0 {
			left--
			continue
		}
		if past == nil {
			past = map[string]bool{}
		}
		past[op.Column] = true
	}
	return past, left
}

// NotNullFromTypeHelp is the operator-facing manual path for a
// not-null-without-default decision whose NOT NULL comes from the column's
// type, typeName, a domain. Neither manual path of NotNullWithoutDefaultHelp
// exists for it: the column cannot be added without NOT NULL, and
// PostgreSQL rewrites the table to check every row against a domain with
// constraints, whatever default the column is given. The NOT NULL may be
// declared by a domain the named one is defined over rather than by the
// named one itself.
func NotNullFromTypeHelp(typeName string) string {
	return "the column's type, " + typeName + ", is a domain that makes it NOT NULL, so the column needs a default, " +
		"and with one PostgreSQL rewrites the table to check every row against the domain; " + domainBaseTypePath
}

// ConstrainedTypeHelp is the operator-facing manual path for adding a
// column whose type, typeName, is a domain with constraints: PostgreSQL
// rewrites the table to check every row against them, whatever default the
// column is given.
func ConstrainedTypeHelp(typeName string) string {
	return "the column's type, " + typeName + ", is a domain with constraints, and PostgreSQL rewrites the table " +
		"to check every row against them whatever default the column is given; " + domainBaseTypePath
}

// domainBaseTypePath is the manual path for adding a column of a domain
// with constraints: the column takes the type the domain chain ends in and
// carries the chain's constraints itself, in forms PostgreSQL adds without
// a rewrite. A domain can be defined over another domain, whose
// constraints it inherits, so the type to use is the first one in the
// chain that is not a domain, and the constraints are those of every
// domain in it.
const domainBaseTypePath = "add the column with the type the domain is defined over instead " +
	"(for a domain over another domain, the first type in the chain that is not a domain), " +
	"with NOT NULL and a constant DEFAULT if a domain in the chain declares NOT NULL, " +
	"and add each CHECK the chain declares as NOT VALID, then VALIDATE CONSTRAINT"

// notNullFromType returns the type op spells for its column when that type
// is what makes the added column NOT NULL: the column spells neither NOT
// NULL nor PRIMARY KEY, so only its type can. It returns "" otherwise.
func notNullFromType(op statement.Op) string {
	if op.DeclaresNotNull || slices.Contains(op.InlineConstraints, statement.ConstraintPrimaryKey) {
		return ""
	}
	return spelledType(op)
}

// spelledType is the added column's type as op spells it, schema-qualified
// when the statement qualifies it, with every identifier quoted so a name
// holding a dot or capitals reads unambiguously.
func spelledType(op statement.Op) string {
	if op.NewTypeSchema != "" {
		return pgx.Identifier{op.NewTypeSchema, op.NewType}.Sanitize()
	}
	return pgx.Identifier{op.NewType}.Sanitize()
}

// NullFillCandidates names the columns the operations of one ALTER TABLE
// add with no DEFAULT of their own, the ones whose existing rows may be
// left NULL: a front door reads Facts.NullFilledColumns only for these.
// The statement need not spell NOT NULL, since the column's domain or an
// inline PRIMARY KEY can make it NOT NULL, so the server's probe decides
// which candidates PostgreSQL would refuse. columns are the table's live
// columns. A column the table already has is skipped by ADD COLUMN IF NOT
// EXISTS, and refused as a duplicate otherwise, so neither fills anything,
// unless an earlier operation of the same statement drops it, which makes
// the add a new column again.
func NullFillCandidates(ops []statement.Op, columns map[string]string) []string {
	dropped := map[string]bool{}
	var candidates []string
	for _, op := range ops {
		if op.Kind == statement.OpDropColumn {
			dropped[op.Column] = true
			continue
		}
		if op.Kind != statement.OpAddColumn || op.Default != statement.DefaultNone || op.GeneratedStored {
			continue
		}
		if _, exists := columns[op.Column]; exists && !dropped[op.Column] {
			continue
		}
		candidates = append(candidates, op.Column)
	}
	return candidates
}

// fillsNotNullWithNull reports whether op adds a NOT NULL column that
// PostgreSQL would refuse because a row of the table would hold NULL in
// it.
func fillsNotNullWithNull(op statement.Op, facts Facts) bool {
	return op.Kind == statement.OpAddColumn && facts.NullFilledColumns[op.Column]
}

// addColumnProof returns what the server proved about adding op's
// column, or "" when no proof was run.
func addColumnProof(op statement.Op, facts Facts) statement.AddColumnProof {
	return facts.AddColumnProofs[op.Column]
}

// addsInPlace reports whether the server proved the column adds without
// rewriting the table, whether or not it then scans it to check a NOT
// NULL.
func addsInPlace(proof statement.AddColumnProof) bool {
	return proof == statement.AddColumnInPlace || proof == statement.AddColumnInPlaceScans ||
		proof == statement.AddColumnInPlaceRunsUserCode
}

// addsOnCatalogAlone reports whether adding op's column is proven to cost
// the same on a table of any size. The probe must prove it adds in place,
// evaluating nothing a user defined (statement.AddColumnInPlace), and the
// default, if any, must be a literal. A default expression is evaluated
// once under the table's lock, and its value can depend on the session,
// such as a setting the probe ran with and the statement does not, so
// what the probe saw is not proof of what the statement will do.
func addsOnCatalogAlone(op statement.Op, facts Facts) bool {
	literalOrNone := op.Default == statement.DefaultNone || op.Default == statement.DefaultConstant
	return literalOrNone && addColumnProof(op, facts) == statement.AddColumnInPlace
}

// ProvenCatalogOnly reports whether every decision is an operation
// PostgreSQL is certain to run on the catalog alone (see
// Decision.ProvenCatalogOnly), so running them costs the same on a table of
// any size. An empty plan proves nothing and reports false.
func ProvenCatalogOnly(decisions []Decision) bool {
	if len(decisions) == 0 {
		return false
	}
	for _, d := range decisions {
		if !d.ProvenCatalogOnly {
			return false
		}
	}
	return true
}

// unprovenRewriteReason names the only cause the statement's shape leaves
// for a rewrite nothing proved: an expression default may be computed for
// every row, and without one only the column's type can rewrite.
func unprovenRewriteReason(op statement.Op) Reason {
	if op.Default == statement.DefaultExpression {
		return ReasonVolatileDefault
	}
	return ReasonTypeRewrite
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
	// PostgreSQL runs every drop of an ALTER TABLE before its other
	// operations, wherever the drop is written, and runs the drops
	// themselves in the order written. So a drop sees only the generated
	// columns the drops written before it remove, and every other
	// operation sees all of them removed.
	afterDrops := withoutDroppedGeneratedColumns(ops, facts)
	// Safer rewrites are only constructed for single-operation statements:
	// a partial rewrite of a multi-operation ALTER would be misleading.
	single := len(ops) == 1
	for i, op := range ops {
		opFacts := afterDrops
		if op.Kind == statement.OpDropColumn {
			opFacts = withoutDroppedGeneratedColumns(ops[:i], facts)
		}
		d := classifyOp(op, st, opFacts, sql, single)
		if op.Kind == statement.OpValidateConstraint && !allTakeShareUpdateExclusiveOnly(ops) {
			// One ALTER TABLE holds the strongest lock any of its
			// operations needs for the whole statement, so the
			// validation scan runs under it: blocking, and only online
			// beside operations that block neither reads nor writes.
			d.Reason = ReasonSaferIdiom
		}
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

// FactsAfter returns facts as they stand once sql has run: the generated
// columns it drops no longer block a later statement's change to the
// columns they read.
func FactsAfter(sql string, facts Facts) (Facts, error) {
	ops, err := statement.ParseOps(sql)
	if err != nil {
		return Facts{}, err
	}
	return withoutDroppedGeneratedColumns(ops, facts), nil
}

// withoutDroppedGeneratedColumns removes, from the generated columns that
// read each column, the copies the drops among ops remove. A copy the drop
// does not reach, such as a child's under ONLY or one the child declares
// itself, stays.
func withoutDroppedGeneratedColumns(ops []statement.Op, facts Facts) Facts {
	dropped := map[string]bool{}
	for _, op := range ops {
		if op.Kind != statement.OpDropColumn {
			continue
		}
		for _, name := range facts.GeneratedColumnCopies.reached(op) {
			dropped[name] = true
		}
	}
	if len(dropped) == 0 {
		return facts
	}
	g := facts.DependentGeneratedColumns
	facts.DependentGeneratedColumns = ColumnDependents{
		Tree: withoutNames(g.Tree, dropped),
		Drop: withoutNames(g.Drop, dropped),
		Own:  withoutNames(g.Own, dropped),
	}
	return facts
}

// withoutNames copies deps without the named dependents, leaving out a
// column none remain for.
func withoutNames(deps map[string][]string, names map[string]bool) map[string][]string {
	out := make(map[string][]string, len(deps))
	for column, objects := range deps {
		kept := slices.DeleteFunc(slices.Clone(objects), func(name string) bool { return names[name] })
		if len(kept) > 0 {
			out[column] = kept
		}
	}
	return out
}

// withoutInheritedCopies leaves out of generated the copies that dropping
// the table's own copy of the same generated column removes. PostgreSQL
// refuses to drop an inherited copy on its own, so naming one would point
// the operator at a drop that fails.
func withoutInheritedCopies(generated []string, copies ColumnDependents) []string {
	named := map[string]bool{}
	for _, name := range generated {
		named[name] = true
	}
	removed := map[string]bool{}
	for column, own := range copies.Own {
		if len(own) != 1 || !named[own[0]] {
			continue
		}
		for _, name := range copies.Drop[column] {
			if name != own[0] {
				removed[name] = true
			}
		}
	}
	if len(removed) == 0 {
		return generated
	}
	return slices.DeleteFunc(slices.Clone(generated), func(name string) bool { return removed[name] })
}

// dependentReason is the refusal reason for the first kind of dependent
// that blocks d, in the order views, policies, triggers, publications,
// generated columns.
func dependentReason(d Decision) (Reason, bool) {
	switch {
	case len(d.DependentViews) > 0:
		return ReasonDependentView, true
	case len(d.DependentPolicies) > 0:
		return ReasonDependentPolicy, true
	case len(d.DependentTriggers) > 0:
		return ReasonDependentTrigger, true
	case len(d.DependentPublications) > 0:
		return ReasonDependentPublication, true
	case len(d.DependentGeneratedColumns) > 0:
		return ReasonDependentGeneratedColumn, true
	}
	return "", false
}

// allTakeShareUpdateExclusiveOnly reports a statement whose every
// operation takes SHARE UPDATE EXCLUSIVE and nothing stronger, so a
// validation scan among them blocks neither reads nor writes.
func allTakeShareUpdateExclusiveOnly(ops []statement.Op) bool {
	for _, op := range ops {
		if !op.TakesShareUpdateExclusiveOnly() {
			return false
		}
	}
	return true
}

// classifyOp routes one operation per the reference table.
func classifyOp(op statement.Op, st statement.Statement, facts Facts, sql string, single bool) Decision {
	d := Decision{Operation: op.Describe(), Kind: op.Kind, Destructive: destructiveOp(op.Kind)}
	if facts.ReplicaIdentity.DroppedBy(op) {
		d.Route, d.Reason = RouteRefuse, ReasonReplicaIdentity
		return d
	}
	if op.Cascade {
		d.Route, d.Reason = RouteRefuse, ReasonCascadingDrop
		return d
	}
	// A partition key comes first among the dependents: no other object
	// can be moved aside to let the change run.
	if tables := facts.PartitionKeys.reached(op); len(tables) > 0 {
		d.Route, d.Reason, d.PartitionKeyTables = RouteRefuse, ReasonPartitionKey, tables
		return d
	}
	// Every kind of dependent that blocks the change is named, so the
	// manual path clears all of them; the reason is the first kind.
	d.DependentViews = viewsReadingColumn(op, facts)
	d.DependentViewDropOrder = viewDropOrder(d.DependentViews, facts.ViewReaders)
	for _, name := range slices.Concat(d.DependentViews, d.DependentViewDropOrder) {
		if slices.Contains(facts.MaterializedViews, name) && !slices.Contains(d.DependentMaterializedViews, name) {
			d.DependentMaterializedViews = append(d.DependentMaterializedViews, name)
		}
	}
	if len(d.DependentMaterializedViews) == 0 {
		d.DependentMaterializedViews = nil
	}
	d.DependentPolicies = policiesReadingColumn(op, facts)
	d.DependentTriggers = facts.DependentTriggers.reached(op)
	d.DependentPublications = facts.DependentPublications.reached(op)
	d.DependentGeneratedColumns = withoutInheritedCopies(facts.DependentGeneratedColumns.reached(op), facts.GeneratedColumnCopies)
	if len(d.DependentViews) > 0 || len(d.DependentPolicies) > 0 || len(d.DependentTriggers) > 0 {
		d.DependentFix = dependentFix(op, facts)
	}
	if reason, blocked := dependentReason(d); blocked {
		d.Route, d.Reason = RouteRefuse, reason
		return d
	}
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
		case facts.ColumnLimitReached[op.Column]:
			// Checked ahead of the adds that route to copy-and-swap only
			// because no backend carries an added column yet. The shadow is
			// a LIKE clone with fresh column numbers, so once a backend does,
			// the copy is the rebuild this refusal's help recommends, and
			// those adds belong ahead of this case.
			numbers := facts.ColumnNumbers
			d.Route, d.Reason, d.ColumnLimit = RouteRefuse, ReasonColumnLimit, &numbers
		case fillsNotNullWithNull(op, facts):
			d.Route, d.Reason, d.NotNullFromType = RouteRefuse, ReasonNotNullWithoutDefault, notNullFromType(op)
		case op.GeneratedStored:
			d.Route, d.Reason = RouteCopyAndSwap, ReasonGeneratedStored
		case addColumnProof(op, facts) == statement.AddColumnDefaultRewrites:
			d.Route, d.Reason = RouteCopyAndSwap, ReasonVolatileDefault
		case addColumnProof(op, facts) == statement.AddColumnTypeRewrites:
			d.Route, d.Reason, d.ConstrainedType = RouteCopyAndSwap, ReasonTypeRewrite, spelledType(op)
		case addColumnProof(op, facts) == statement.AddColumnUnproven:
			d.Route, d.Reason, d.Unverified = RouteCopyAndSwap, unprovenRewriteReason(op), true
		case op.Default == statement.DefaultExpression && !addsInPlace(addColumnProof(op, facts)):
			// No proof was run, as in an offline lint: the default may be
			// computed for every row.
			d.Route, d.Reason, d.Unverified = RouteCopyAndSwap, ReasonVolatileDefault, true
		case len(op.InlineConstraints) > 0:
			// An inline UNIQUE / PRIMARY KEY / FOREIGN KEY / CHECK does the
			// same index build or validation as its ADD CONSTRAINT form,
			// under the ADD COLUMN's ACCESS EXCLUSIVE lock. The safer path
			// splits the column addition from an online constraint build;
			// the planner does not construct multi-statement splits, so the
			// decision carries no rewrite and routing fails closed.
			d.Route, d.Reason = RouteNative, ReasonSaferIdiom
		case op.Default != statement.DefaultNone:
			// A constant, or an expression the server proved it stores
			// in the catalog.
			d.Route, d.Reason = RouteNative, ReasonFastDefault
			d.ProvenCatalogOnly = addsOnCatalogAlone(op, facts)
		default:
			d.Route, d.Reason = RouteNative, ReasonMetadataOnly
			d.ProvenCatalogOnly = addsOnCatalogAlone(op, facts)
		}

	case statement.OpDropColumn, statement.OpDropDefault,
		statement.OpDropNotNull, statement.OpDropConstraint:
		// PostgreSQL never touches the table's rows for these and runs no
		// user code: a dropped column is only marked dropped, a dropped
		// default applies to rows written later, and dropping a NOT NULL
		// or a constraint has nothing to check. An index the drop takes
		// with it is removed as a file, not read.
		d.Route, d.Reason = RouteNative, ReasonMetadataOnly
		d.ProvenCatalogOnly = true

	case statement.OpSetDefault:
		// The new default applies to rows written later, so no row is
		// read. It is still not proven catalog-only: the server coerces
		// the expression to the column's type under the lock, and for a
		// user-defined base type that runs the type's input function,
		// which can read any table. The planner has no fact that rules
		// that out, so the statement stays size-guarded.
		d.Route, d.Reason = RouteNative, ReasonMetadataOnly

	case statement.OpSetColumnOptions, statement.OpSetRelOptions,
		statement.OpSetSchema:
		d.Route, d.Reason = RouteNative, ReasonMetadataOnly

	case statement.OpRenameColumn, statement.OpRenameTable:
		// Metadata-only for PostgreSQL, but not for the application: a
		// rename cannot land atomically across deployed instances, so
		// code querying the old name breaks the instant it commits. The
		// submitted form is not executable as submitted, so the router
		// marks the statement rewrite-required and the typed guidance
		// names the safe sequence; the acknowledged override is the one
		// way to run the rename itself.
		d.Route, d.Reason = RouteNative, ReasonAppBreakingRename

	case statement.OpRenameIndex:
		// Ordinary SQL names tables and columns, not indexes, so an index
		// rename is metadata-only. An index that backs a primary-key,
		// unique, or exclusion constraint is the exception: renaming it
		// renames the constraint, which ON CONFLICT ON CONSTRAINT and
		// error handling name, so it breaks the application like a column
		// rename does. Without the live fact — an offline lint — the
		// rename classifies as metadata-only, and the executor re-checks
		// the constraint under the table's lock before it runs. Either way
		// the rename changes only the catalog's names: it reads no row of
		// the table and runs no user code.
		d.Route, d.Reason = RouteNative, ReasonMetadataOnly
		d.ProvenCatalogOnly = true
		if facts.RenamedIndexConstraint != "" {
			d.Reason = ReasonAppBreakingRename
		}

	case statement.OpAlterColumnType:
		d.Route, d.Reason, d.Unverified = classifyTypeChange(op, facts)
		if swapsTimeZone(op, facts) {
			d.ReadsInTimeZone = facts.SessionTimeZone
		}

	case statement.OpSetNotNull:
		// Native pattern: prove the invariant with a NOT VALID CHECK plus
		// an online VALIDATE, then SET NOT NULL is a catalog flip (PG 12+).
		d.Route, d.Reason = RouteNative, ReasonSaferIdiom
		if single {
			d.SaferSQL = setNotNullSequence(st, op.Column, facts.NotNullHelpers[op.Column])
		}

	case statement.OpSetTablespace:
		d.Route, d.Reason, d.Unverified = classifySetTablespace(facts.Storage)

	case statement.OpAddConstraint:
		d = classifyAddConstraint(op, st, sql, single)
		if len(d.SaferSQL) > 0 && validatedOnAdd(op) {
			// Restated from the live facts, which name an unnamed
			// constraint as the server would on this table.
			d.SaferSQL = notValidSequence(st, sql, facts)
		}

	case statement.OpValidateConstraint:
		d.Route, d.Reason = RouteNative, ReasonOnlineIdiom

	case statement.OpAttachPartition:
		// Native pattern, stated by suggest.GuidancePrevalidatedCheck. The
		// planner cannot construct its CHECKs from the statement, so it
		// routes native without a rewrite.
		d.Route, d.Reason = RouteNative, ReasonSaferIdiom

	case statement.OpDetachPartition:
		d = concurrentlyDecision(d, op.Concurrent, sql, single)

	case statement.OpCreateIndex, statement.OpDropIndex, statement.OpReindex:
		d = concurrentlyDecision(d, op.Concurrent, sql, single)

	case statement.OpAddEnumValue:
		// Adding a label writes one catalog row under a lock on the type
		// alone: no table that uses the type is locked, scanned, or
		// rewritten, and the existing labels keep their order. Sessions
		// can use the new label once the statement commits.
		d.Route, d.Reason = RouteNative, ReasonMetadataOnly

	default:
		d.Route, d.Reason = RouteRefuse, ReasonUnsupportedOperation
	}
	return d
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
	d := Decision{Operation: op.Describe(), Kind: op.Kind}
	switch {
	case op.UsingIndex, op.NotValid:
		// Already the safe pattern's cheap step.
		d.Route, d.Reason = RouteNative, ReasonOnlineIdiom

	case op.Constraint == statement.ConstraintPrimaryKey,
		op.Constraint == statement.ConstraintUnique:
		// Direct ADD PK/UNIQUE builds its index under ACCESS EXCLUSIVE;
		// the safer sequence builds it concurrently and attaches it.
		d.Route, d.Reason = RouteNative, ReasonSaferIdiom
		if canBuildUsingIndexSequence(single, op) {
			d.SaferSQL = usingIndexSequence(st, op)
		}

	case validatedOnAdd(op):
		// Direct ADD CHECK/FK validates under ACCESS EXCLUSIVE; the safer
		// sequence is NOT VALID plus an online VALIDATE. An unnamed
		// constraint gets the server's first choice here, from the
		// statement alone; classifyOp restates the sequence from the live
		// facts.
		d.Route, d.Reason = RouteNative, ReasonSaferIdiom
		if single {
			d.SaferSQL = notValidSequence(st, sql, Facts{})
		}

	case op.Constraint == statement.ConstraintNotNull:
		d.Route, d.Reason = RouteNative, ReasonSaferIdiom

	default:
		// EXCLUDE and anything unrecognized: no online pattern exists.
		d.Route, d.Reason = RouteRefuse, ReasonUnsupportedOperation
	}
	return d
}

// validatedOnAdd reports a CHECK or FOREIGN KEY, the constraints ADD
// CONSTRAINT validates against every row unless told NOT VALID.
func validatedOnAdd(op statement.Op) bool {
	return op.Constraint == statement.ConstraintCheck || op.Constraint == statement.ConstraintForeignKey
}

// notValidSequence is the safer sequence for a single ADD CHECK or FOREIGN
// KEY: the constraint added NOT VALID, then validated online. It is nil
// when the statement cannot take NOT VALID.
func notValidSequence(st statement.Statement, sql string, facts Facts) []string {
	notValid, name, err := statement.AddNotValid(sql, constraintNaming(facts))
	if err != nil {
		return nil
	}
	return []string{
		notValid,
		"ALTER TABLE " + tableIdent(st) + " VALIDATE CONSTRAINT " + pgx.Identifier{name}.Sanitize(),
	}
}

// constraintNaming is what facts tell the rewrite about naming an unnamed
// constraint: the live columns, and the names the schema already uses.
func constraintNaming(facts Facts) statement.ConstraintNaming {
	naming := statement.ConstraintNaming{Taken: facts.ConstraintNames}
	if facts.ColumnTypes != nil {
		naming.Columns = make(map[string]bool, len(facts.ColumnTypes))
		for column := range facts.ColumnTypes {
			naming.Columns[column] = true
		}
	}
	return naming
}

// classifySetTablespace routes SET TABLESPACE by the table's storage. A
// heap moves to the new tablespace, a rewrite-scale copy under ACCESS
// EXCLUSIVE. A partitioned parent has no heap to move: the server records
// the tablespace as the default for partitions created later, under a
// brief ACCESS EXCLUSIVE on the parent alone, and moves no existing
// partition or index. Unread storage fails closed to the move, and says so.
func classifySetTablespace(storage TableStorage) (Route, Reason, bool) {
	switch storage {
	case StoragePartitionedParent:
		return RouteNative, ReasonMetadataOnly, false
	case StorageHeap:
		return RouteCopyAndSwap, ReasonRelocation, false
	case StorageUnread:
		return RouteCopyAndSwap, ReasonRelocation, true
	}
	return RouteCopyAndSwap, ReasonRelocation, true
}

// classifyTypeChange routes ALTER COLUMN TYPE: binary-coercible changes are
// a brief catalog relabel; everything else — or anything the planner cannot
// verify against live column facts — is a rewrite.
func classifyTypeChange(op statement.Op, facts Facts) (Route, Reason, bool) {
	if op.HasUsing {
		return RouteCopyAndSwap, ReasonTypeRewrite, false
	}
	collationKept := false
	if live, collated := facts.ColumnCollations[op.Column]; collated {
		// The live column has a collation of its own. A type change with
		// no COLLATE clause resets it to the new type's default, and one
		// naming another collation replaces it; either way every index on
		// the column is rebuilt. A clause naming the live collation keeps
		// it, and the type rules decide.
		if !keepsCollation(live, op.NewTypeCollationName) {
			return RouteCopyAndSwap, ReasonTypeRewrite, false
		}
		collationKept = true
	}
	oldType, ok := facts.ColumnTypes[op.Column]
	if !ok {
		// No live type for the column: fail closed to the rewrite route,
		// but say so — the conversion may be a free relabel that a fact
		// would prove.
		return RouteCopyAndSwap, ReasonTypeRewrite, true
	}
	old, next := parseTypeText(oldType), targetTypeShape(op, collationKept)
	if timeZoneSwap(old, next) {
		return classifyTimeZoneSwap(op, next, facts)
	}
	if binaryCoercible(old, next) {
		return RouteNative, ReasonBinaryCoercible, false
	}
	return RouteCopyAndSwap, ReasonTypeRewrite, false
}

// classifyTimeZoneSwap classifies a change between timestamp and
// timestamptz. PostgreSQL reads each stored value in the session time
// zone, and keeps the stored bytes only when that zone is UTC: in any
// other zone every value moves by the zone's offset and the table is
// rewritten. Even in UTC every index on the column is rebuilt under the
// change's exclusive lock, since the index's operator class changes with
// the type, and a foreign key the column is part of is checked again with
// a scan, since its equality operator changes too. A precision below the
// maximum rounds every value, and an array's elements are converted one by
// one. So the change is a relabel only in UTC, for a scalar column no
// index or foreign key reads, to an unspecified or maximum precision. A
// zone or a key that was not read fails closed to the rewrite route,
// marked unverified. The executor checks the zone again inside the
// change's own transaction.
func classifyTimeZoneSwap(op statement.Op, next typeShape, facts Facts) (Route, Reason, bool) {
	if op.HasUsing || next.array || !keepsTemporalPrecision(nil, next.mods) {
		return RouteCopyAndSwap, ReasonTypeRewrite, false
	}
	if facts.SessionTimeZone == "" {
		return RouteCopyAndSwap, ReasonTypeRewrite, true
	}
	if !preflight.UTCTimeZone(facts.SessionTimeZone) {
		return RouteCopyAndSwap, ReasonTypeRewrite, false
	}
	if facts.KeyedColumns == nil {
		return RouteCopyAndSwap, ReasonTypeRewrite, true
	}
	if facts.KeyedColumns[op.Column] {
		return RouteCopyAndSwap, ReasonTypeRewrite, false
	}
	return RouteNative, ReasonBinaryCoercible, false
}

// swapsTimeZone reports whether op changes a column between timestamp and
// timestamptz, reading each stored value in the session time zone: a
// USING expression decides the conversion itself.
func swapsTimeZone(op statement.Op, facts Facts) bool {
	if op.Kind != statement.OpAlterColumnType || op.HasUsing {
		return false
	}
	live, ok := facts.ColumnTypes[op.Column]
	if !ok {
		return false
	}
	return timeZoneSwap(parseTypeText(live), targetTypeShape(op, true))
}

// timeZoneSwap reports whether old to next changes timestamp to
// timestamptz or timestamptz to timestamp, as scalars or as arrays.
func timeZoneSwap(old, next typeShape) bool {
	if old.array != next.array {
		return false
	}
	return (old.name == "timestamp" && next.name == "timestamptz") ||
		(old.name == "timestamptz" && next.name == "timestamp")
}

// typeShape is a normalized type family plus its modifiers and whether it
// is an array of that family, comparable across the grammar's spelling and
// format_type's rendering.
type typeShape struct {
	name  string
	mods  []int32
	array bool
}

// keepsCollation reports whether a type change's COLLATE clause names the
// live column's collation, rendered schema-qualified as schemadiff reads
// it. An unqualified name keeps a pg_catalog collation of that name, since
// the engine's sessions search pg_catalog first; a collation in any other
// schema is kept only by a clause qualified with that schema, since an
// unqualified name resolves through the search path. No clause, or a live
// collation that does not parse, keeps nothing.
func keepsCollation(live string, stated statement.QualifiedName) bool {
	if stated.Name == "" {
		return false
	}
	liveName, err := statement.ParseCollationName(live)
	if err != nil {
		return false
	}
	if stated.Schema == "" {
		return liveName.Schema == "pg_catalog" && liveName.Name == stated.Name
	}
	return liveName == stated
}

// targetTypeShape reads the statement's target type into the shape
// parseTypeText gives the live one. A type qualified with a schema other
// than pg_catalog is a user type that only shares a built-in's name, and a
// COLLATE clause that changes the collation changes the one every index on
// the column is built with, so neither gets a shape and neither matches a
// rule. collationKept says the clause names the live collation. The name is
// taken as the parser left it: the grammar already spells every built-in
// the rules know by its catalog name (varchar, numeric, timestamptz), so a
// target named "decimal" or "character varying" was quoted and names a
// user type.
func targetTypeShape(op statement.Op, collationKept bool) typeShape {
	if op.NewTypeSchema != "" || (op.NewTypeCollation != "" && !collationKept) {
		return typeShape{}
	}
	return typeShape{name: strings.TrimSpace(op.NewType), mods: op.NewTypeMods, array: op.NewTypeArray}
}

// binaryCoercible reports whether changing old to new is a relabel
// PostgreSQL performs without a rewrite or scan. The rules are the
// reference table's rows, deliberately narrow: widening varchar, varchar to
// text, widening numeric precision at the same scale, widening or removing
// the precision of timestamp, timestamptz, time, and timetz, widening or
// removing a varbit length, and cidr to inet. Anything not provably on this
// list is not coercible.
//
// The widening rules hold for scalar columns only. An array column's
// elements are converted one by one, and PostgreSQL rewrites the table for
// any element change, widenings included; only an unchanged element type
// is a relabel.
func binaryCoercible(old, next typeShape) bool {
	if old.name == "" || next.name == "" {
		return false
	}
	if old.array != next.array {
		return false
	}
	if old.name == next.name && int32sEqual(old.mods, next.mods) {
		return true // no-op relabel
	}
	if old.array {
		return false
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
	case "timestamp", "timestamptz", "time", "timetz":
		return next.name == old.name && keepsTemporalPrecision(old.mods, next.mods)
	case "varbit":
		if next.name != "varbit" {
			return false
		}
		if len(next.mods) == 0 {
			return true // dropping the length bound
		}
		return len(old.mods) == 1 && len(next.mods) == 1 && next.mods[0] >= old.mods[0]
	case "cidr":
		// A cidr value is an inet value with no host bits set, stored the
		// same way, so the cast is a relabel. The reverse masks host bits
		// and rewrites.
		return next.name == "inet" && len(next.mods) == 0
	default:
		return false
	}
}

// maxTemporalPrecision is the largest fractional-seconds precision
// PostgreSQL stores for timestamp, timestamptz, time, and timetz; an
// unspecified precision means the same.
const maxTemporalPrecision = 6

// keepsTemporalPrecision reports whether a temporal type's new precision
// keeps every stored value as it is: unspecified, the maximum, or at least
// the old precision. PostgreSQL relabels those without a rewrite, and
// rounds every value, rewriting the table, for a lower one.
func keepsTemporalPrecision(old, next []int32) bool {
	if len(next) == 0 {
		return true
	}
	if len(next) != 1 {
		return false
	}
	if next[0] >= maxTemporalPrecision {
		return true
	}
	return len(old) == 1 && next[0] >= old[0]
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

// catalogTypeName maps format_type's SQL-standard spelling of a built-in
// onto the catalog name the grammar gives the same type, so a live type and
// a target type compare by name. Unknown names pass through unchanged (and
// will never match a rule).
//
// The match is case-sensitive on purpose. format_type renders the catalog's
// name, and a name that still carries upper case was quoted, so "TEXT"
// names a user type, not the built-in text, and folding it would let that
// type pass as the built-in.
func catalogTypeName(name string) string {
	switch name = strings.TrimSpace(name); name {
	case "character varying":
		return "varchar"
	case "timestamp without time zone":
		return "timestamp"
	case "timestamp with time zone":
		return "timestamptz"
	case "time without time zone":
		return "time"
	case "time with time zone":
		return "timetz"
	case "bit varying":
		return "varbit"
	default:
		return name
	}
}

// parseTypeText splits a format_type rendering ("character varying(50)",
// "numeric(10,2)", "text", "character varying(50)[]",
// "timestamp(3) with time zone") into its normalized shape. This reads
// catalog output, not SQL — format_type's rendering is stable, and it marks
// every array with one trailing "[]" whatever the declared dimensions. The
// modifiers can sit inside the name, so whatever follows them is part of
// the name: "timestamp(3) with time zone" is timestamptz, not timestamp.
func parseTypeText(s string) typeShape {
	s, array := strings.CutSuffix(strings.TrimSpace(s), "[]")
	name, rest, found := strings.Cut(s, "(")
	if !found {
		return typeShape{name: catalogTypeName(name), array: array}
	}
	mods, suffix, found := strings.Cut(rest, ")")
	if !found {
		return typeShape{}
	}
	shape := typeShape{name: catalogTypeName(name + suffix), array: array}
	for part := range strings.SplitSeq(mods, ",") {
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
// The scaffold is named <table>_<column>_not_null_check, never
// <table>_<column>_not_null: from PostgreSQL 18, SET NOT NULL records a
// NOT NULL constraint under that second name, and while the scaffold held
// it the server would pick the next free one instead. The converged table
// would then carry a different constraint name from the same table built
// directly from its definition.
//
// Partial-failure contract: step 1 leaves a NOT VALID CHECK under the
// generated scaffold name, and re-running step 1 then fails with SQLSTATE
// 42710 (duplicate_object) — a retry resumes at step 2. A failed VALIDATE
// (step 2) leaves the same scaffold and is safe to re-run. Steps 3 and 4
// are metadata-only and safe to re-run; a leftover scaffold is removed by
// running step 4 alone.
//
// helperLive is set when the scaffold is already on the live table, left by
// a run that stopped before step 4 (Facts.NotNullHelpers). The sequence then
// starts at step 2: validating a scaffold that a run already validated
// checks no rows, and the scaffold checks new writes until step 4 drops it,
// as it did while that run was in progress.
func setNotNullSequence(st statement.Statement, column string, helperLive bool) []string {
	table := tableIdent(st)
	con := pgx.Identifier{NotNullHelperName(st.Table(), column)}.Sanitize()
	col := pgx.Identifier{column}.Sanitize()
	steps := []string{
		fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s IS NOT NULL) NOT VALID", table, con, col),
		fmt.Sprintf("ALTER TABLE %s VALIDATE CONSTRAINT %s", table, con),
		fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET NOT NULL", table, col),
		fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s", table, con),
	}
	if helperLive {
		return steps[1:]
	}
	return steps
}

// NotNullHelperName is the name of the helper CHECK the online SET NOT NULL
// sequence adds to table for column, fitted to PostgreSQL's identifier
// limit.
func NotNullHelperName(table, column string) string {
	return fitIdentifier(fmt.Sprintf("%s_%s_not_null_check", table, column))
}

// canBuildUsingIndexSequence reports whether an ADD PRIMARY KEY / UNIQUE
// can be rewritten to the two-step USING INDEX sequence: the statement
// has this one operation, the key names its columns, and every storage
// parameter renders faithfully. A sequence that left a parameter out
// would build a different index than the statement declares, so such a
// key gets guidance instead.
func canBuildUsingIndexSequence(single bool, op statement.Op) bool {
	return single && len(op.Columns) > 0 && !op.IndexOptionsUnrenderable
}

// usingIndexSequence is the native two-step ADD PRIMARY KEY / UNIQUE
// pattern: build the unique index concurrently, then attach it as the
// constraint under a brief lock. The sequence must declare the same
// constraint the statement does, so the index carries the INCLUDE columns,
// NULLS NOT DISTINCT, and the tablespace, and the attach carries
// DEFERRABLE and INITIALLY DEFERRED; a key built without one of them
// accepts rows, or checks them at a moment, the statement did not ask for.
// The index carries the key's WITH (...) storage parameters too; the
// caller builds no sequence when one cannot be rendered faithfully
// (canBuildUsingIndexSequence).
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
	create := fmt.Sprintf("CREATE UNIQUE INDEX CONCURRENTLY %s ON %s (%s)", idx, table, strings.Join(cols, ", "))
	if len(op.IncludeColumns) > 0 {
		include := make([]string, len(op.IncludeColumns))
		for i, c := range op.IncludeColumns {
			include[i] = pgx.Identifier{c}.Sanitize()
		}
		create += " INCLUDE (" + strings.Join(include, ", ") + ")"
	}
	if op.NullsNotDistinct {
		create += " NULLS NOT DISTINCT"
	}
	if len(op.IndexStorageParams) > 0 {
		params := make([]string, len(op.IndexStorageParams))
		for i, p := range op.IndexStorageParams {
			params[i] = pgx.Identifier{p.Name}.Sanitize()
			if p.HasValue {
				params[i] += " = '" + strings.ReplaceAll(p.Value, "'", "''") + "'"
			}
		}
		create += " WITH (" + strings.Join(params, ", ") + ")"
	}
	if op.IndexTablespace != "" {
		create += " TABLESPACE " + pgx.Identifier{op.IndexTablespace}.Sanitize()
	}
	attach := fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s %s USING INDEX %s", table, idx, keyword, idx)
	if op.Deferrable {
		attach += " DEFERRABLE"
	}
	if op.InitiallyDeferred {
		attach += " INITIALLY DEFERRED"
	}
	return []string{create, attach}
}
