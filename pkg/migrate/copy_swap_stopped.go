package migrate

import (
	"context"
	"fmt"

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
}

// findStoppedRun reads the checkpoint row of the resolved table
// schema.table through schemachange.ResumableRow. No table (a statement
// that names none), no row, a terminal row, and a row this engine cannot
// read are no stopped run, as they are to the copy-and-swap route's own
// preflight: the route decides those itself under the table's lock. A
// read that fails is returned and nothing runs.
func findStoppedRun(ctx context.Context, pool *pgxpool.Pool, schema, table string) (stoppedRun, error) {
	if schema == "" || table == "" {
		return stoppedRun{}, nil
	}
	store, err := checkpoint.NewStore(pool, checkpoint.Options{})
	if err != nil {
		return stoppedRun{}, err
	}
	cp, found, err := schemachange.ResumableRow(ctx, store, schema, table)
	if err != nil {
		return stoppedRun{}, fmt.Errorf("look for a stopped copy-and-swap run on %s.%s: %w", schema, table, err)
	}
	return stoppedRun{cp: cp, found: found}, nil
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
	if !s.found || s.resumedBy(st) {
		return false
	}
	return opts.CopySwap.Restart == ""
}

// copySwapRunStoppedVerdict is the imperative door's refusal of st over
// the stopped run, naming the run and the remedy. A run whose row is past
// cutover may have committed its swap, which a restart does not discard,
// so only finishing it is offered there.
func copySwapRunStoppedVerdict(st statement.Statement, s stoppedRun) verdict.Verdict {
	detail := fmt.Sprintf("a copy-and-swap run of %q on %s stopped in phase %s, and this statement is not the one it records; "+
		"run the stopped statement again to finish it, then run this one",
		s.cp.Statement, qualified(st), s.cp.Phase)
	if s.cp.Phase != checkpoint.PhaseCutover {
		detail += ", or, when this statement routes to copy-and-swap, start it over with the restart acknowledgement " +
			"(--restart SCHEMA.TABLE, CopySwapOptions.Restart), which discards the stopped run first"
	}
	return refusedVerdict(st, copySwapRunStoppedRefusal(), detail+"; nothing was executed")
}

// desiredRunStoppedDetail is the declarative door's refusal detail for a
// table that already has the desired shape while a copy-and-swap run on it
// is stopped: reporting it converged would leave the stopped run's row and
// what it holds standing with nothing to finish them, and desired-state
// execution takes no acknowledgement to discard them.
func desiredRunStoppedDetail(schema, table string, s stoppedRun) string {
	return fmt.Sprintf("the live table matches the desired schema, but a copy-and-swap run of %q on %s.%s stopped in phase %s; "+
		"run that statement again with migrate --alter to finish it; nothing was executed",
		s.cp.Statement, schema, table, s.cp.Phase)
}
