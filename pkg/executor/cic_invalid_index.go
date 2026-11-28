package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// InvalidIndexError reports that an invalid index exists (or may remain)
// and the executor will not or cannot remove it: the one outcome that
// needs an operator. Build carries the failure that produced the leftover,
// nil when there is no build failure to carry (the entry predates this
// run, or a reported success failed its validity verification); Cleanup
// carries why automatic recovery was refused, failed, or could not be
// proven.
type InvalidIndexError struct {
	// Schema and Index identify the possibly-invalid index.
	Schema string
	Index  string
	// Table is the table the invalid index sits on, when the catalog
	// inspection saw the entry; empty when the state could not be
	// inspected.
	Table string
	// BuilderPID is the backend running a concurrent build or reindex of
	// this exact index (pg_stat_progress_create_index), when the entry is
	// in flight; zero otherwise.
	BuilderPID uint32
	// Ready is pg_index.indisready for the entry, as the statement that
	// found it read it. The server maintains a ready index on every write,
	// so a ready unique index rejects writes that duplicate its key even
	// while it is invalid. A concurrent build that failed before its entry
	// became ready leaves an index no write touches. ReadinessUnknown when
	// no statement read the entry: the drop or its proof failed before the
	// catalog was read, or the state could not be inspected.
	Ready IndexReadiness
	// Build is the build failure that left the index invalid; nil when
	// there is no build failure to carry.
	Build error
	// Cleanup is why the recovery drop was refused, failed, or could not
	// be verified.
	Cleanup error
}

// Recoverable reports whether RebuildAbandonedIndex can remove the entry
// and build the requested index: true for this build's own proven
// leftover, for an abandoned entry on the target table, and for an entry
// on the target table whose builder this role cannot observe — the
// recovery's proof is a table lock, not the progress view, so it decides
// what the view could not. A visibly in-flight build, another table's
// debris, an index the server will not drop concurrently, and an unproven
// state are not this actor's to recover. A build that failed on a data
// fault is not recoverable either, whatever it left: the rebuild would
// meet the same rows and fail the same way, so the rows are corrected
// first and the leftover removed as the error says. Nor is a build that
// failed because the server ran out of a resource: the rebuild meets the
// same limit until an operator frees the resource or raises the limit.
func (e *InvalidIndexError) Recoverable() bool {
	switch {
	case e.buildDataFault():
		return false
	case isResourceExhausted(e.Build):
		return false
	case errors.Is(e.Cleanup, ErrBuildLeftInvalidIndex):
		return true
	case errors.Is(e.Cleanup, ErrAbandonedInvalidIndex):
		return true
	case errors.Is(e.Cleanup, ErrInvalidIndexBuilderUnobservable):
		return true
	default:
		return false
	}
}

// Error implements the error interface. The advice is as state-specific as
// the type: a removal is named only in the states where the recovery can
// prove ownership before it drops — the same standard the executor holds
// itself to. An in-flight build says wait; another table's debris, an
// index the server will not drop concurrently, and an unproven state get
// investigation steps, never a statement to copy-paste: the index under
// that name may be healthy, or may be exactly what it is meant to be. A
// build that failed on a data fault leads with the fault, since no
// recovery of the leftover gets past the rows (dataFaultError).
func (e *InvalidIndexError) Error() string {
	name := fmt.Sprintf("%s.%s", e.Schema, e.Index)
	switch {
	case e.buildDataFault():
		return e.dataFaultError(name)
	case isResourceExhausted(e.Build):
		return e.resourceExhaustedError(name)
	case errors.Is(e.Cleanup, ErrBuildLeftInvalidIndex):
		return fmt.Sprintf("index %s is this build's own invalid leftover%s; recover with RebuildAbandonedIndex or DROP INDEX CONCURRENTLY %s, see docs/invalid-index-recovery.md: %v",
			name, e.buildClause(), pgx.Identifier{e.Schema, e.Index}.Sanitize(), e.Cleanup)
	case errors.Is(e.Cleanup, ErrInvalidIndexBuildInFlight):
		return fmt.Sprintf("index %s is invalid because backend %d is still building it%s; wait for that build to finish or fail, see docs/invalid-index-recovery.md: %v",
			name, e.BuilderPID, e.buildClause(), e.Cleanup)
	case errors.Is(e.Cleanup, ErrAbandonedInvalidIndex):
		return fmt.Sprintf("index %s is abandoned invalid debris on table %s with no backend building it%s; recover with RebuildAbandonedIndex, see docs/invalid-index-recovery.md: %v",
			name, e.Table, e.buildClause(), e.Cleanup)
	case errors.Is(e.Cleanup, ErrInvalidIndexOnOtherTable):
		return fmt.Sprintf("index %s is invalid debris on a different table (%s)%s, which this change will not remove; recover it from that table's own change or inspect it yourself, see docs/invalid-index-recovery.md: %v",
			name, e.Table, e.buildClause(), e.Cleanup)
	case errors.Is(e.Cleanup, ErrInvalidIndexNotDroppable):
		return fmt.Sprintf("index %s is invalid on table %s but is not debris the server will remove concurrently (a partitioned table's index, an index partition, or a constraint's index)%s; this change leaves it in place — inspect pg_class.relkind, pg_class.relispartition, and pg_constraint.conindid yourself, see docs/invalid-index-recovery.md: %v",
			name, e.Table, e.buildClause(), e.Cleanup)
	case errors.Is(e.Cleanup, ErrInvalidIndexBuilderUnobservable):
		return fmt.Sprintf("index %s is invalid on table %s and this role cannot see whether another backend is still building it%s; recover with RebuildAbandonedIndex, which proves the state under the table lock, or inspect pg_stat_progress_create_index as a role with pg_read_all_stats, see docs/invalid-index-recovery.md: %v",
			name, e.Table, e.buildClause(), e.Cleanup)
	default:
		return fmt.Sprintf("index %s may be invalid but the catalog state could not be proven%s; inspect pg_index.indisvalid and pg_stat_progress_create_index yourself before any recovery, see docs/invalid-index-recovery.md: %v",
			name, e.buildClause(), e.Cleanup)
	}
}

// buildDataFault reports whether the build failed because the server
// rejected the table's rows (SQLSTATE class 22 or 23): duplicate values
// under a unique index, or an index expression that raises on a row. Every
// rerun of the same build meets the same rows until they are corrected. A
// violation the server reports on a pg_catalog relation is not the rows':
// two builds that pick one index name in one schema collide in pg_class's
// unique name index, and the loser's leftover is judged by Cleanup instead.
func (e *InvalidIndexError) buildDataFault() bool {
	return isDataFault(e.Build) && !violatesCatalog(e.Build)
}

// violatesCatalog reports whether err is the server rejecting a write to
// one of its own catalogs rather than to the target table.
func violatesCatalog(err error) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	return ok && pgErr.SchemaName == catalogSchema
}

// dataFaultError renders a build that failed on its rows. The server error
// rides in the message and in the chain, where a unique violation's detail
// names the duplicated key. The leftover is still reported: the build's
// own invalid index occupies the name and blocks a rerun, so its proven
// removal is named once the rows are corrected; an entry this run could not
// prove its own gets investigation steps instead, as in Error.
func (e *InvalidIndexError) dataFaultError(name string) string {
	fault := fmt.Sprintf("index %s was not built because the table's rows violate it (%v); every rerun meets the same rows until the data is corrected",
		name, e.Build)
	if errors.Is(e.Cleanup, ErrBuildLeftInvalidIndex) {
		return fmt.Sprintf("%s; the failed build left its own invalid index on table %s.%s, which blocks a rerun under the same name: correct the rows, then DROP INDEX CONCURRENTLY %s and run the change again, see docs/invalid-index-recovery.md: %v",
			fault, e.Schema, e.Table, pgx.Identifier{e.Schema, e.Index}.Sanitize(), e.Cleanup)
	}
	return fmt.Sprintf("%s; an invalid index remains under the name that this run could not prove its own; correct the rows, and inspect pg_index.indisvalid and pg_stat_progress_create_index before any removal, see docs/invalid-index-recovery.md: %v",
		fault, e.Cleanup)
}

// resourceExhaustedError renders a build that failed because the server ran
// out of a resource or hit a configured limit. The server error rides in the
// message, since it names the resource. The leftover is reported as in
// dataFaultError: its proven removal is named once the resource is freed, and
// an entry this run could not prove its own gets investigation steps.
func (e *InvalidIndexError) resourceExhaustedError(name string) string {
	fault := fmt.Sprintf("index %s was not built because the server ran out of a resource or hit a configured limit (%v); every rerun fails the same way until the resource is freed or the limit raised",
		name, e.Build)
	if errors.Is(e.Cleanup, ErrBuildLeftInvalidIndex) {
		return fmt.Sprintf("%s; the failed build left its own invalid index on table %s.%s, which blocks a rerun under the same name: free the resource or raise the limit, then DROP INDEX CONCURRENTLY %s and run the change again, see docs/invalid-index-recovery.md: %v",
			fault, e.Schema, e.Table, pgx.Identifier{e.Schema, e.Index}.Sanitize(), e.Cleanup)
	}
	return fmt.Sprintf("%s; an invalid index remains under the name that this run could not prove its own; free the resource or raise the limit, and inspect pg_index.indisvalid and pg_stat_progress_create_index before any removal, see docs/invalid-index-recovery.md: %v",
		fault, e.Cleanup)
}

// buildClause names the build failure that left the index invalid, so the
// server's error reaches whoever reads the message; empty when no build
// failure is carried.
func (e *InvalidIndexError) buildClause() string {
	if e.Build == nil {
		return ""
	}
	return fmt.Sprintf(" (the build failed: %v)", e.Build)
}

// Unwrap exposes the underlying failures to errors.Is/As.
func (e *InvalidIndexError) Unwrap() []error {
	errs := make([]error, 0, 2)
	if e.Build != nil {
		errs = append(errs, e.Build)
	}
	if e.Cleanup != nil {
		errs = append(errs, e.Cleanup)
	}
	return errs
}

// invalidIndex is one catalog observation of an invalid index: its
// identity (the OID a later proof re-checks), the table it sits on,
// whether the server could remove it concurrently, and what the snapshot
// says about a backend building it.
type invalidIndex struct {
	oid      uint32
	tableOID uint32
	table    string
	// droppable reports whether DROP INDEX CONCURRENTLY can remove the
	// entry (droppableColumn). An entry it cannot remove is not the debris
	// of a failed concurrent build, whatever its validity flag says.
	droppable bool
	// ready is pg_index.indisready: whether the server maintains the entry
	// on every write.
	ready   IndexReadiness
	builder builderFacts
}

// IndexReadiness is pg_index.indisready for an invalid index, as a
// statement read it, or ReadinessUnknown when none did. A caller that words
// a leftover by whether writes still maintain it checks for
// ReadinessUnknown first and words that case without a claim either way.
type IndexReadiness int

const (
	// ReadinessUnknown is the zero value: no statement read indisready
	// for the entry.
	ReadinessUnknown IndexReadiness = iota
	// ReadinessNotReady is indisready false: no write maintains the
	// entry, so it enforces nothing.
	ReadinessNotReady
	// ReadinessReady is indisready true: every write maintains the entry,
	// so a unique one rejects writes that duplicate its key.
	ReadinessReady
)

// String names the readiness for logs and messages.
func (r IndexReadiness) String() string {
	switch r {
	case ReadinessNotReady:
		return "not ready"
	case ReadinessReady:
		return "ready"
	default:
		return "unknown"
	}
}

// readinessOf turns indisready as a statement read it into its readiness.
func readinessOf(indisready bool) IndexReadiness {
	if indisready {
		return ReadinessReady
	}
	return ReadinessNotReady
}

// droppableColumn is the boolean that says whether DROP INDEX CONCURRENTLY
// can remove the index whose pg_class row is aliased c. The server refuses
// three shapes, and each is an index that is invalid for a reason other
// than a failed concurrent build: a partitioned table's index (relkind I,
// invalid by design until every partition's index is attached), an index
// partition attached to such a parent (relispartition), and an index
// backing a constraint — unique, primary key, exclusion, or referenced by a
// foreign key — which pg_constraint.conindid names.
const droppableColumn = `(c.relkind OPERATOR(pg_catalog.=) 'i'
		            AND NOT c.relispartition
		            AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint k
		                             WHERE k.conindid OPERATOR(pg_catalog.=) c.oid))`

// builderFacts is what one catalog snapshot says about the backend building
// an index, together with whether that answer can be trusted. A visible
// builder is positive evidence; its absence is evidence only when this
// session could have seen one.
type builderFacts struct {
	// pid is the backend whose concurrent build or reindex is reported
	// against the index OID by pg_stat_progress_create_index; zero when
	// none is visible.
	pid uint32
	// hiddenRows counts the concurrent index commands in this database
	// whose target the progress view withholds from this session: another
	// role's command shows only its PID and database to a reader without
	// pg_read_all_stats. Any such row may be the builder of the entry
	// under inspection.
	hiddenRows int64
	// tracking reports this session's track_activities. A backend records
	// no progress row while the setting is off, so an off setting here —
	// the same server default the builder most likely runs under — means
	// the view's silence proves nothing.
	tracking bool
}

// observable reports whether a zero pid means no backend is building the
// entry: every command in the database is visible to this session and
// commands are being recorded at all.
func (f builderFacts) observable() bool {
	return f.tracking && f.hiddenRows == 0
}

// builderPIDSubquery is the scalar subquery that names the backend
// building the index whose pg_class row is aliased c. PostgreSQL reports
// a concurrent build's index OID in pg_stat_progress_create_index as soon
// as the catalog entry is visible, and the row disappears when the
// command ends, so a NULL here means no backend is visibly building the
// entry right now — builderVisibilityColumns says whether that visibility
// can be trusted. It is scoped to the current database because progress
// rows are cluster-wide while OIDs are not.
const builderPIDSubquery = `(SELECT p.pid
		           FROM pg_catalog.pg_stat_progress_create_index p
		          WHERE p.datname OPERATOR(pg_catalog.=) pg_catalog.current_database()
		            AND p.index_relid OPERATOR(pg_catalog.=) c.oid
		          ORDER BY p.pid
		          LIMIT 1)`

// builderVisibilityColumns are the two facts that say whether
// builderPIDSubquery's silence is trustworthy, read in the same snapshot:
// the number of progress rows in this database whose target relation the
// view withholds from this session (a visible command always reports its
// table, so a NULL there is a hidden command, not a command that has yet
// to create its index), and whether this session records activity at all.
// The hidden-row count is database-wide by necessity, not by choice: what
// the view withholds is precisely the command's target, so a hidden row
// cannot be attributed to any table, and any one of them may be building
// the entry under inspection. An unrelated hidden build elsewhere in the
// database therefore does turn an observable silence into an unobservable
// one — the honest answer, since this session cannot tell them apart.
const builderVisibilityColumns = `(SELECT pg_catalog.count(*)
		           FROM pg_catalog.pg_stat_progress_create_index p
		          WHERE p.datname OPERATOR(pg_catalog.=) pg_catalog.current_database()
		            AND p.relid IS NULL),
		        pg_catalog.current_setting('track_activities') OPERATOR(pg_catalog.=) 'on'`

// newBuilderFacts assembles the scanned builder columns. Backend PIDs are
// positive, so the nullable-PID conversion is lossless.
func newBuilderFacts(pid *int32, hiddenRows int64, tracking bool) builderFacts {
	return builderFacts{pid: pidValue(pid), hiddenRows: hiddenRows, tracking: tracking}
}

// inspectInvalidIndex reports whether an index named index exists anywhere
// in the schema and is marked invalid (pg_index.indisvalid = false) — the
// state a failed concurrent build leaves behind, and the state a
// concurrent build in progress is in. The check is deliberately
// schema-wide rather than pinned to one table: debris carries the
// requested name in the table's schema whatever table it ended up on, and
// an invalid index under this name on any table makes a later failure
// verdict undecidable. The identity, table, readiness, and builder facts
// come from one statement, so they describe the same entry.
func inspectInvalidIndex(ctx context.Context, q querier, schema, index string) (invalidIndex, bool, error) {
	var (
		found      invalidIndex
		valid      bool
		indisready bool
		builderPID *int32
		hiddenRows int64
		tracking   bool
	)
	err := q.QueryRow(ctx,
		`SELECT c.oid, i.indisvalid, i.indisready, i.indrelid, t.relname, `+droppableColumn+`, `+builderPIDSubquery+`, `+builderVisibilityColumns+`
		   FROM pg_catalog.pg_index i
		   JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) i.indexrelid
		   JOIN pg_catalog.pg_class t ON t.oid OPERATOR(pg_catalog.=) i.indrelid
		   JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		  WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) $2`,
		schema, index).Scan(&found.oid, &valid, &indisready, &found.tableOID, &found.table, &found.droppable, &builderPID, &hiddenRows, &tracking)
	if errors.Is(err, pgx.ErrNoRows) {
		return invalidIndex{}, false, nil
	}
	if err != nil {
		return invalidIndex{}, false, fmt.Errorf("inspect index %s.%s: %w", schema, index, err)
	}
	if valid {
		return invalidIndex{}, false, nil
	}
	found.ready = readinessOf(indisready)
	found.builder = newBuilderFacts(builderPID, hiddenRows, tracking)
	return found, true, nil
}

// pidValue turns a nullable backend PID into the zero-means-none form the
// error types carry. Backend PIDs are positive, so the conversion is
// lossless.
func pidValue(pid *int32) uint32 {
	if pid == nil || *pid <= 0 {
		return 0
	}
	return uint32(*pid)
}

// classifyInvalidIndex turns an observed invalid index under the given
// name into the typed refusal the build, the failure verdict, and the
// recovery all act on. The order is the order of proof strength: a visible
// builder is positive evidence the entry is not abandoned whatever table it
// is on; without one, the table decides whether the debris is this change's
// to recover; an entry the server will not drop concurrently is not the
// debris of a failed concurrent build at all; and only then does
// visibility decide — silence is the given verdict only when this session
// could have seen a builder and saw none. The silence verdict is the one
// fact the callers disagree on: before a build the entry is anonymous
// abandoned debris (ErrAbandonedInvalidIndex); after this build's own
// failure it is this build's leftover (ErrBuildLeftInvalidIndex).
func classifyInvalidIndex(existing invalidIndex, target indexTarget, index string, silence error) *InvalidIndexError {
	e := &InvalidIndexError{Schema: target.schema, Index: index, Table: existing.table, BuilderPID: existing.builder.pid, Ready: existing.ready}
	switch {
	case existing.builder.pid != 0:
		e.Cleanup = ErrInvalidIndexBuildInFlight
	case existing.tableOID != target.tableOID:
		e.Cleanup = ErrInvalidIndexOnOtherTable
	case !existing.droppable:
		e.Cleanup = ErrInvalidIndexNotDroppable
	case !existing.builder.observable():
		e.Cleanup = ErrInvalidIndexBuilderUnobservable
	default:
		e.Cleanup = silence
	}
	return e
}
