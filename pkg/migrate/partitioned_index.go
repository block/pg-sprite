package migrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// ErrConnectionRequired is returned by [Run] and [RunDesired] when a
// statement is a CREATE INDEX on a partitioned table, or by [Run] when it is
// an ADD PRIMARY KEY or ADD UNIQUE on one, and Options.Connection is unset.
// The partitioned index and key builds take the parent's table lock on a
// dedicated session of their own, so they need the connection the pool was
// dialed with, not the pool alone; a library caller that builds Options by
// hand wires it, the CLI always does. It is a sentinel so an embedder can tell
// the missing wiring apart from an operational failure with [errors.Is].
// Nothing has been executed when it is returned: [RunDesired] checks it for
// the whole plan before its first statement, and [Run] before the statement's
// first step.
var ErrConnectionRequired = errors.New(
	"Options.Connection must name the database the pool was dialed with; the partitioned index and key builds open their table lock session from it")

// PartitionedIndexConnectionMismatchError is returned by [Run] and
// [RunDesired] when Options.Connection reaches a database other than the one
// the pool was dialed with. The build takes its table lock on a session opened
// from the connection and runs every other statement on the pool. An advisory
// lock excludes only sessions of the database it was taken in, so a lock taken
// in another database would not keep a second run off the table. Nothing has
// been executed when it is returned; it is a wiring mistake that no retry
// corrects, so match it with [errors.AsType] and fix the wiring rather than
// routing it as an outcome. A database is told from another by the cluster's
// system identifier together with the database name, which is unique only
// within a cluster. Both identifiers are stable fingerprints of a server, so a
// caller that posts results to a shared surface logs the fields and shows a
// fixed line rather than the raw text.
type PartitionedIndexConnectionMismatchError struct {
	// PoolCluster and PoolDatabase identify the database the pool reaches.
	PoolCluster  string
	PoolDatabase string
	// ConnectionCluster and ConnectionDatabase identify the database
	// Options.Connection reaches.
	ConnectionCluster  string
	ConnectionDatabase string
}

// Error implements the error interface.
func (e *PartitionedIndexConnectionMismatchError) Error() string {
	if e.PoolCluster != e.ConnectionCluster {
		return fmt.Sprintf("Options.Connection reaches database %q on cluster %s, the pool database %q on cluster %s; both must name the database the pool was dialed with",
			e.ConnectionDatabase, e.ConnectionCluster, e.PoolDatabase, e.PoolCluster)
	}
	return fmt.Sprintf("Options.Connection reaches database %q, the pool database %q on the same cluster; both must name the database the pool was dialed with",
		e.ConnectionDatabase, e.PoolDatabase)
}

// partitionedIndexDatabase is what tells one session's database from another's:
// the cluster's system identifier and the database name within it.
type partitionedIndexDatabase struct {
	cluster  string
	database string
}

const partitionedIndexDatabaseSQL = `
	SELECT s.system_identifier::pg_catalog.text, pg_catalog.current_database()
	FROM pg_catalog.pg_control_system() s`

func readPartitionedIndexDatabase(ctx context.Context, session dbconn.RowQuerier) (partitionedIndexDatabase, error) {
	var id partitionedIndexDatabase
	err := session.QueryRow(ctx, partitionedIndexDatabaseSQL).Scan(&id.cluster, &id.database)
	return id, err
}

// checkPartitionedIndexConnection proves the partitioned index build's wiring
// before it opens anything: Options.Connection is set ([ErrConnectionRequired])
// and a session dialed from it is in the pool's database
// ([PartitionedIndexConnectionMismatchError]). A session that could not be
// opened or identified is an ordinary error, since nothing is known about where
// it would have landed. RunDesired asks it during whole-plan admission, and the
// build asks it again when it runs.
// INV: LK-1 — the lock session is proven to be in the pool's database before
// it is opened, so the lock it takes excludes the runs that work this table.
func checkPartitionedIndexConnection(ctx context.Context, pool *pgxpool.Pool, opts Options) (err error) {
	if opts.Connection.URL == "" {
		return ErrConnectionRequired
	}
	poolID, err := readPartitionedIndexDatabase(ctx, pool)
	if err != nil {
		return fmt.Errorf("identify the pool's database: %w", err)
	}
	conn, err := dbconn.ConnectSession(ctx, opts.Connection)
	if err != nil {
		return fmt.Errorf("open a session from Options.Connection: %w", err)
	}
	defer func() {
		if closeErr := conn.Close(context.WithoutCancel(ctx)); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close the session opened from Options.Connection: %w", closeErr))
		}
	}()
	connID, err := readPartitionedIndexDatabase(ctx, conn)
	if err != nil {
		return fmt.Errorf("identify the database Options.Connection reaches: %w", err)
	}
	if connID != poolID {
		return &PartitionedIndexConnectionMismatchError{
			PoolCluster: poolID.cluster, PoolDatabase: poolID.database,
			ConnectionCluster: connID.cluster, ConnectionDatabase: connID.database,
		}
	}
	return nil
}

// partitionedIndexBuild runs a CREATE INDEX on a partitioned table as the
// partition-aware index build and ends in one verdict. The statement is the
// request itself, not the planner's steps: the substitute the planner made
// for a plain table, a concurrent build, cannot run on a partitioned parent.
// The partitions are listed when the build runs. A failure leaves the parent's
// invalid index and the partition indexes built so far; running the same
// statement again resumes from them, and the failed verdict says so.
func partitionedIndexBuild(ctx context.Context, pool *pgxpool.Pool, pt preflight.PreflightedTable,
	st statement.Statement, opts Options, step int, logger *slog.Logger) (verdict.Verdict, error) {
	if err := checkPartitionedIndexConnection(ctx, pool, opts); err != nil {
		return verdict.Verdict{}, fmt.Errorf("migrate: %w", err)
	}
	logger.Debug("routing a CREATE INDEX on a partitioned table to the partitioned index build",
		"table", qualified(st))
	start := time.Now()
	rep, err := executor.BuildPartitionedIndexAsStep(ctx, opts.Connection, pool, pt, st.SQL(),
		opts.Budget.Concurrent, opts.runStep(step))
	elapsed := time.Since(start)
	if err != nil {
		if v, refused := execRefusal(st, err, true, false, true); refused {
			logger.Debug("partitioned index build refused",
				"reason", string(v.Reason), "elapsed", elapsed)
			return v, nil
		}
		v := partitionedIndexFailureVerdict(st, err)
		logger.Debug("partitioned index build failed", "code", v.Code, "elapsed", elapsed)
		return v, fmt.Errorf("run schema change on %s: %w", qualified(st), err)
	}
	logger.Debug("partitioned index build committed",
		"table", qualified(st), "index", rep.Index, "partitions", rep.Partitions, "worked", len(rep.Worked), "elapsed", elapsed)
	return verdict.Verdict{
		Outcome:   verdict.OutcomeExecuted,
		Statement: st.SQL(),
		Table:     qualified(st),
		Detail:    partitionedIndexDetail(rep),
	}, nil
}

// partitionedIndexDetail says what a finished build did, in one sentence.
func partitionedIndexDetail(rep executor.PartitionedIndexReport) string {
	return fmt.Sprintf("the table is partitioned, so pg-sprite built index %s partition by partition and attached each one: "+
		"%d partitions, %d given an index by this run; the index is valid",
		rep.Index, rep.Partitions, len(rep.Worked))
}

// partitionedIndexFailureVerdict is the failed verdict for a partitioned
// index build that stopped. The build never rolls back what it finished, and
// the error alone does not say how much that was: a refusal can come from the
// first catalog read or from a later one after indexes had committed. The
// detail therefore claims neither that nothing changed nor what remains; it
// says earlier steps may have committed and how to find out, and that running
// the same statement again resumes from whatever the build had finished.
func partitionedIndexFailureVerdict(st statement.Statement, err error) verdict.Verdict {
	v := verdict.Verdict{
		Outcome:   verdict.OutcomeFailed,
		Code:      string(executor.OutcomeCode(err)),
		Statement: st.SQL(),
		Table:     qualified(st),
	}
	if _, commitUnknown := errors.AsType[*executor.CommitOutcomeUnknownError](err); commitUnknown {
		v.Detail = fmt.Sprintf("outcome unknown: a COMMIT did not return success, so whether the last step of the partitioned index build committed "+
			"cannot be established from the client; inspect the indexes of %s and its partitions, then run the same statement again to resume", v.Table)
		return v
	}
	v.Detail = fmt.Sprintf("the partitioned index build stopped before it finished; steps before the stop may have committed, "+
		"so inspect the indexes of %s and its partitions, then run the same statement again to resume from whatever it had built", v.Table)
	return v
}
