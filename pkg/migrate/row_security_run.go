package migrate

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

var (
	// errAtomicRowSecurityPin rejects a pin on the atomic row security
	// change, which derives its statements again under its own lock and
	// has no plan to pin.
	errAtomicRowSecurityPin = errors.New("migrate: RowSecurityRequest.ExpectedFingerprint does not apply to a row security change: " +
		"it runs in one transaction under its own lock and has no plan to pin; pin a change to the table definition alone through RunDesiredWithRowSecurity")
	// errAtomicRowSecurityDestructive rejects a destructive
	// acknowledgement on the atomic row security change, which
	// acknowledges nothing.
	errAtomicRowSecurityDestructive = errors.New("migrate: RowSecurityRequest.AllowDestructive does not apply to a row security change: " +
		"it runs in one transaction and acknowledges nothing; it applies to a change to the table definition alone through RunDesiredWithRowSecurity")
)

// RunRowSecurity converges a desired file that declares row security in one
// transaction, as executor.ExecuteDesiredRowSecurity converges it: an
// existing table's row security is replaced as executor.ExecuteRowSecurity
// replaces it, and an absent table is created with its row security, owned
// by Options.CreateOwner when that is set. It is the door a host sends a
// row security change through, since the executors read no copy-and-swap
// checkpoint row:
//
//   - It takes the table's lock on a session opened from
//     Options.Connection, the lock a copy-and-swap run holds for its whole
//     life, and never waits for it. A held lock — a copy-and-swap run, a
//     discard, or another row security change at work on the table — ends
//     the call at once in a failed verdict, with an error matching
//     *dbconn.TableLockHeldError beside it and nothing executed, as [Run]
//     reports a copy-and-swap run that finds the lock held.
//   - Holding the lock, it reads the table's checkpoint row as [Run] does,
//     and a copy-and-swap run stopped on the table refuses the change with
//     cause copy-and-swap-run-stopped, the verdict
//     [AdmitRowSecurityStoppedRun] forecasts, and nothing runs. No run can
//     start on the table between the read and the change's commit.
//   - It runs the change under Options.Budget.Brief, cancelled if the lock
//     is lost, and lets the lock go, bounded, however the change ended.
//
// Options.Connection must name the pool's database, as for a partitioned
// index build: unset is [ErrConnectionRequired] and another database a
// [*PartitionedIndexConnectionMismatchError], before anything is opened.
// The lock's session is outside the pool, so the pool needs only the one
// connection the change's transaction runs on; the server needs one more
// for the lock's session, and another beside it while that session proves
// its affinity. The lock's session reads its own start from
// pg_stat_activity, as for a copy-and-swap run, so a connection that sets a
// role other than the one it logs in as needs that role to hold
// pg_read_all_stats.
//
// Like the rest of desired-state execution it takes no acknowledgement:
// Options carrying one is rejected as [RunDesired] rejects it, and so are
// RowSecurityRequest.ExpectedFingerprint and AllowDestructive, which bind a
// table definition's plan. Options.Progress is not reported to.
//
// The verdict says what happened, as the CLI prints it. A refusal over a
// stopped run returns no error. Otherwise the executor's error, refused or
// failed, is returned beside its verdict so a host can match the typed
// cause; a failed commit is not evidence of rollback and is reported as
// row-security-outcome-unknown. An error with the zero verdict means the
// call stopped before it reached the table: a wiring mistake, a session it
// could not open, or a checkpoint row it could not read.
func RunRowSecurity(ctx context.Context, pool *pgxpool.Pool, req RowSecurityRequest, opts Options) (verdict.Verdict, error) {
	return runRowSecurityLocked(ctx, pool, req, nil, opts)
}

// RunReviewedRowSecurity is [RunRowSecurity] bound to reviewed SQL, as
// executor.ExecuteReviewedRowSecurity binds it: the change to the existing
// table's row security is derived again under the table's own lock and
// refused with executor.ErrRowSecurityPlanChanged unless it is exactly
// reviewed, in order and with duplicates; an empty reviewed authorizes only
// a no-op. The reviewed sequence is executor.PreviewRowSecurity's. The
// table's advisory lock, the stopped-run check under it, the wiring, and
// the result are RunRowSecurity's. A table that does not exist is not
// created: Options.CreateOwner does not apply.
func RunReviewedRowSecurity(ctx context.Context, pool *pgxpool.Pool, req RowSecurityRequest, reviewed []string, opts Options) (verdict.Verdict, error) {
	if reviewed == nil {
		// nil names the desired-state change below; a review of nothing
		// admits only a no-op, as ExecuteReviewedRowSecurity takes it.
		reviewed = []string{}
	}
	return runRowSecurityLocked(ctx, pool, req, reviewed, opts)
}

// runRowSecurityLocked runs the row security change on req's table under
// the table's advisory lock once the checkpoint row, read under that lock,
// shows no copy-and-swap run stopped on the table. A nil reviewed runs the
// desired-state change; any other reviewed, empty included, runs the
// reviewed one.
func runRowSecurityLocked(ctx context.Context, pool *pgxpool.Pool, req RowSecurityRequest, reviewed []string, opts Options) (v verdict.Verdict, err error) {
	if err := checkAtomicRowSecurity(req, opts); err != nil {
		return verdict.Verdict{}, err
	}
	schema, table := req.Schema, req.Desired.Table()
	target := schema + "." + table
	if schema == "" || table == "" {
		// The executor's own refusal, given before a lock with no table
		// to name is attempted.
		refused := fmt.Errorf("%w: %w", executor.ErrRowSecurityRefused, statement.ErrRowSecurityDeclaration)
		return rowSecurityVerdict(target, executor.RowSecurityReport{}, refused), refused
	}
	if err := checkPartitionedIndexConnection(ctx, pool, opts); err != nil {
		return verdict.Verdict{}, fmt.Errorf("migrate: %w", err)
	}
	// INV: LK-1 — the change holds the lock a copy-and-swap run holds for
	// its whole life, so no run starts on the table until it commits; a
	// held lock is refused at once, never waited for.
	lock, err := dbconn.AcquireTableLock(ctx, opts.Connection, schema, table)
	if _, held := errors.AsType[*dbconn.TableLockHeldError](err); held {
		return tableLockHeldVerdict(target, err), fmt.Errorf("migrate: row security on %s: %w", target, err)
	}
	if err != nil {
		return verdict.Verdict{}, fmt.Errorf("migrate: row security on %s: %w", target, err)
	}
	defer func() { v, err = releaseRowSecurityLock(ctx, lock, target, v, err) }()
	locked, stop := lock.Bind(ctx)
	defer stop()
	// INV: ST-2 — the checkpoint row is read under the table's lock, so the
	// stopped run it shows, or its absence, holds until the change commits.
	stopped, err := findStoppedRun(locked, pool, schema, table)
	if err != nil {
		return verdict.Verdict{}, err
	}
	if stopped.standing() {
		return rowSecurityStoppedRunVerdict(schema, table, stopped), nil
	}
	// The executor is called here, after the stopped-run read under the
	// lock, so the door walk sees the check ahead of the change.
	var report executor.RowSecurityReport
	var runErr error
	if reviewed == nil {
		report, runErr = executor.ExecuteDesiredRowSecurity(locked, pool, schema, opts.CreateOwner, req.Desired, opts.Budget.Brief)
	} else {
		report, runErr = executor.ExecuteReviewedRowSecurity(locked, pool, schema, req.Desired, reviewed, opts.Budget.Brief)
	}
	return rowSecurityVerdict(target, report, runErr), runErr
}

// checkAtomicRowSecurity rejects, before any database work, what the
// atomic row security change cannot honour.
func checkAtomicRowSecurity(req RowSecurityRequest, opts Options) error {
	// INV: RF-12 — desired-state execution takes no acknowledgement in
	// Options.
	if err := rejectDesiredAcks(opts); err != nil {
		return err
	}
	if err := opts.validate(); err != nil {
		return err
	}
	if req.ExpectedFingerprint != "" {
		return errAtomicRowSecurityPin
	}
	if req.AllowDestructive != "" {
		return errAtomicRowSecurityDestructive
	}
	return nil
}

// tableLockHeldVerdict is the failed verdict for a row security change on
// table whose advisory lock another session holds, err naming the holder.
func tableLockHeldVerdict(table string, err error) verdict.Verdict {
	return verdict.Verdict{
		Table:   table,
		Outcome: verdict.OutcomeFailed,
		Code:    string(executor.OutcomeCode(err)),
		Detail: err.Error() + ": a copy-and-swap run, a discard, or another row security change is at work on the table; " +
			"nothing was executed; run the change again once it has ended",
	}
}

// releaseRowSecurityLock lets the table's lock go once the change has
// ended, as Runner.Run lets its lock go: the release outlives ctx, bounded,
// and a session that lost the lock has closed itself. A lost lock or a
// failed release is joined to err, except beside a change that committed,
// whose verdict stays executed and names it, since the change is done.
func releaseRowSecurityLock(ctx context.Context, lock *dbconn.TableLockSession, table string, v verdict.Verdict, err error) (verdict.Verdict, error) {
	lockErr := lock.Err()
	if lockErr == nil {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbconn.DefaultConnectTimeout)
		defer cancel()
		lockErr = lock.Release(releaseCtx)
	}
	if lockErr == nil {
		return v, err
	}
	if v.Outcome == verdict.OutcomeExecuted {
		v.Detail += "; the change committed, but the table's lock did not end cleanly: " + lockErr.Error()
		return v, err
	}
	return v, errors.Join(err, fmt.Errorf("migrate: table lock on %s: %w", table, lockErr))
}

// A failed commit is not evidence of rollback. Preserve its distinct code and
// explanation; never disclose uncommitted statements as executed SQL.
func rowSecurityVerdict(table string, report executor.RowSecurityReport, err error) verdict.Verdict {
	v := verdict.Verdict{Table: table, Outcome: verdict.OutcomeExecuted, ExecutedSQL: report.Statements,
		Detail: "row security converged: all changes committed in one transaction"}
	if err == nil {
		if report.TableCreated {
			v.TableCreated = true
			v.ExtensionSchemas = report.ExtensionSchemas
			v.Detail = "table created with its row security: all changes committed in one transaction"
		}
		if len(report.Statements) == 0 {
			v.Detail = "already converged: row security matches; nothing to run"
		}
		return v
	}
	v.Outcome = verdict.OutcomeFailed
	v.ExecutedSQL = nil
	v.Detail = err.Error()
	if refusal, ok := RowSecurityRefusal(err); ok {
		return v.WithRefusal(refusal)
	}
	v.Code = string(executor.OutcomeCode(err))
	return WithServerError(v, err)
}
