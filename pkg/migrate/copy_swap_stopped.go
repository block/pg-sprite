package migrate

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// stoppedRun is the copy-and-swap run a table's checkpoint row records as
// stopped short of done, read before the front door sends a statement on
// the table anywhere. The router decides a route from the table's current
// shape, and a stopped run may already have changed that shape — a run
// whose swap committed has put the statement's shape live — so a
// statement can route natively while a stopped run still holds a row, a
// shadow or the retained source, and a fence or a slot. The row, not the
// route, decides what runs next.
type stoppedRun struct {
	cp    checkpoint.Checkpoint
	found bool
	// unjudged, when set, says why the table has a run this engine cannot
	// judge: its row cannot be read or is not one this engine understands,
	// and the run's shadow or retained source stands in the catalog. Such
	// a run is never resumed here and holds the table like any other.
	unjudged string
	// leftover is the standing relation that shows the unjudged run.
	leftover string
}

// findStoppedRun reads the checkpoint row of the resolved table
// schema.table, without the table's lock: a forecast, which a Runner
// judges again under the lock. No table (a statement that names none), no row, no
// checkpoint store, and a terminal row are no stopped run. A row the read
// cannot judge — the store refuses this role (42501), the store was shaped
// by an older engine, or the row is in another format or a phase this
// engine does not know — is a stopped run exactly when the catalog, which
// every role can read, still shows the run's shadow or retained source on
// the table: the native route reads no row and takes no lock, so nothing
// later would decide it. Any other read that fails is returned and nothing
// runs.
func findStoppedRun(ctx context.Context, pool *pgxpool.Pool, schema, table string) (stoppedRun, error) {
	if schema == "" || table == "" {
		return stoppedRun{}, nil
	}
	store, err := checkpoint.NewStore(pool, checkpoint.Options{})
	if err != nil {
		return stoppedRun{}, err
	}
	cp, err := store.Inspect(ctx, schema, table)
	switch {
	case err == nil:
		return stoppedRun{cp: cp, found: !cp.Phase.Terminal()}, nil
	case errors.Is(err, checkpoint.ErrNotFound), errors.Is(err, checkpoint.ErrTableMissing):
		return stoppedRun{}, nil
	case errors.Is(err, checkpoint.ErrTableBehind):
		return unjudgedRun(ctx, pool, schema, table, "the checkpoint store was created by an older pg-sprite")
	case isPermissionDenied(err):
		return unjudgedRun(ctx, pool, schema, table, "this role may not read the checkpoint store")
	}
	if _, ok := errors.AsType[*checkpoint.IncompatibleError](err); ok {
		return unjudgedRun(ctx, pool, schema, table, "its checkpoint row is in a format or phase this pg-sprite does not know")
	}
	return stoppedRun{}, fmt.Errorf("look for a stopped copy-and-swap run on %s.%s: %w", schema, table, err)
}

// unjudgedRun looks in the catalog for the relations a copy-and-swap run
// of schema.table leaves standing until it is finished or discarded — its
// shadow before the swap, the retained source after it — and reports an
// unjudged run, for reason, when one stands. A run stopped before its
// shadow exists holds nothing a native statement could change under it.
func unjudgedRun(ctx context.Context, pool *pgxpool.Pool, schema, table, reason string) (stoppedRun, error) {
	for _, name := range []string{schemachange.ShadowName(schema, table), schemachange.OldName(schema, table)} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT pg_catalog.to_regclass($1) IS NOT NULL`,
			pgx.Identifier{schema, name}.Sanitize()).Scan(&exists); err != nil {
			return stoppedRun{}, fmt.Errorf("look for a stopped copy-and-swap run's %s on %s.%s: %w", name, schema, table, err)
		}
		if exists {
			return stoppedRun{unjudged: reason, leftover: name}, nil
		}
	}
	return stoppedRun{}, nil
}

// standing reports whether a stopped run holds the table, judged or not.
func (s stoppedRun) standing() bool {
	return s.found || s.unjudged != ""
}

// isPermissionDenied reports whether err is the server refusing the read
// for want of a privilege.
func isPermissionDenied(err error) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	return ok && pgErr.Code == sqlstateInsufficientPrivilege
}

// resumedBy reports whether st is the statement the stopped run records,
// compared as parsed, the way the Runner compares them when it resumes:
// the same operations on the same table, whatever the spelling. A row
// that records no statement is resumed by none, and the Runner would
// refuse it too.
func (s stoppedRun) resumedBy(st statement.Statement) bool {
	if !s.found || s.cp.Statement == "" {
		return false
	}
	return statement.SameOpsExceptTarget(s.cp.Statement, st.SQL()) == nil
}

// blocks reports whether the stopped run keeps st from running: there is
// one, st is not its statement, and no restart acknowledgement discards
// it. Under the acknowledgement st routes as it would without the row,
// and the copy-and-swap route discards the stopped run before it starts.
func (s stoppedRun) blocks(st statement.Statement, opts Options) bool {
	if !s.standing() || s.resumedBy(st) {
		return false
	}
	return opts.CopySwap.Restart == ""
}

// refuseOverStoppedRun is the check for a statement that names an index
// rather than its table — an index rename, or a blocking index statement
// run under --accept-blocking — once its owning table schema.table is
// known. Such a statement never resumes or restarts a copy-and-swap run,
// so a stopped run on the table refuses it, as it refuses any other
// statement.
func refuseOverStoppedRun(ctx context.Context, pool *pgxpool.Pool, st statement.Statement, schema, table string) (verdict.Verdict, bool, error) {
	stopped, err := findStoppedRun(ctx, pool, schema, table)
	if err != nil {
		return verdict.Verdict{}, false, err
	}
	if !stopped.standing() {
		return verdict.Verdict{}, false, nil
	}
	return copySwapRunStoppedVerdict(st, schema+"."+table, stopped), true, nil
}

// copySwapRunStoppedVerdict is the imperative door's refusal of st over
// the stopped run on table, naming the run and the remedy. A run whose row
// is past cutover may have committed its swap, which a restart does not
// discard, so only finishing it is offered there.
func copySwapRunStoppedVerdict(st statement.Statement, table string, s stoppedRun) verdict.Verdict {
	v := refusedVerdict(st, copySwapRunStoppedRefusal(), stoppedRunDetail(table, s)+"; nothing was executed")
	v.Table = table
	return v
}

// stoppedRunDetail names the stopped run on table and the way past it.
func stoppedRunDetail(table string, s stoppedRun) string {
	if s.unjudged != "" {
		return fmt.Sprintf("a copy-and-swap run on %s left %s standing, and this pg-sprite cannot judge it: %s; "+
			"finish or discard that run with a pg-sprite and a role that can read its checkpoint row, then run this statement",
			table, s.leftover, s.unjudged)
	}
	detail := fmt.Sprintf("a copy-and-swap run of %q on %s stopped in phase %s, and this statement is not the one it records; "+
		"run the stopped statement again to finish it, then run this one",
		s.cp.Statement, table, s.cp.Phase)
	if s.cp.Phase != checkpoint.PhaseCutover {
		detail += ", or, when this statement routes to copy-and-swap, start it over with the restart acknowledgement " +
			"and the route opened (--restart SCHEMA.TABLE with --copy-and-swap; CopySwapOptions.Restart with Enabled), " +
			"which discards the stopped run first"
	}
	return detail
}

// desiredRunStoppedDetail is the declarative door's refusal detail for a
// table that already has the desired shape while a copy-and-swap run on it
// is stopped: reporting it converged would leave the stopped run's row and
// what it holds standing with nothing to finish them, and desired-state
// execution takes no acknowledgement to discard them.
func desiredRunStoppedDetail(schema, table string, s stoppedRun) string {
	if s.unjudged != "" {
		return "the live table matches the desired schema, but " + stoppedRunDetail(schema+"."+table, s) + "; nothing was executed"
	}
	return fmt.Sprintf("the live table matches the desired schema, but a copy-and-swap run of %q on %s.%s stopped in phase %s; "+
		"run that statement again with migrate --alter to finish it; nothing was executed",
		s.cp.Statement, schema, table, s.cp.Phase)
}
