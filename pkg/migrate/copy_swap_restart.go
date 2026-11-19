package migrate

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/statement"
)

// ErrRestartNotSupported is returned by [RunDesired] when
// Options.CopySwap.Restart is set. The acknowledgement names one run to
// discard, but on the declarative front door the derived plan, not the
// operator, decides which statements take copy-and-swap and so which run
// the acknowledgement would discard; a value left on an embedder's
// configuration would throw away a stopped run's checkpoint, shadow, and
// fence or slot on every later plan, and a plan whose statements route
// elsewhere would fail on the acknowledgement after an earlier statement
// committed. Starting a run over belongs on the imperative front door,
// where the operator names the statement and the table together.
var ErrRestartNotSupported = errors.New(
	"the restart acknowledgement applies to the imperative front door only; desired-state execution resumes a stopped run and never starts one over")

// ErrRestartMismatch means the restart acknowledgement named a table other
// than the statement's resolved target. Nothing has executed.
var ErrRestartMismatch = errors.New("the restart acknowledgement must name the resolved target table")

// checkRestartAck confirms that the restart acknowledgement names the
// resolved target table, exactly as the force acknowledgement must: a
// typed table name is what stops the flag from being left on a script and
// discarding the next table's run by accident.
func checkRestartAck(st statement.Statement, ack string) error {
	if ack == qualified(st) {
		return nil
	}
	return fmt.Errorf("%w %q, got %q; nothing was executed", ErrRestartMismatch, qualified(st), ack)
}

// checkRestartRoute confirms that a statement acknowledged for a restart
// is one the copy-and-swap backend executes. Starting over is a
// copy-and-swap notion — a shadow, a fence or a slot, a checkpoint row —
// and a statement the router sends anywhere else has nothing of the kind
// to discard, so the acknowledgement is an error rather than a no-op: an
// operator who typed it expected a leftover to go.
func checkRestartRoute(st statement.Statement, rs router.Statement) error {
	if rs.Disposition == router.DispositionExecute && rs.Backend == router.BackendCopyAndSwap {
		return nil
	}
	return fmt.Errorf("the restart acknowledgement applies to a statement the copy-and-swap backend executes; the statement on %s routes to backend %q with disposition %q; nothing was executed",
		qualified(st), string(rs.Backend), string(rs.Disposition))
}

// auditRestart records the operator's decision to discard an earlier run
// before anything is discarded. The record is warn-level and
// unconditional, as the force audit is: an audit trail must not depend on
// diagnostics being enabled.
func auditRestart(audit *slog.Logger, st statement.Statement) {
	audit.Warn("restart acknowledged; an earlier run on the table will be discarded",
		"table", qualified(st),
		"kind", st.Kind().String())
}
