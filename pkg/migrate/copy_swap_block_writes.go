package migrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/statement"
)

// sourceHold is what a copy-and-swap run holds on its source while it is
// stopped short of done, and so what its verdict says the operator is
// paying for until the run is resumed or started over.
type sourceHold int

const (
	// holdSlot is a live run's replication slot, which pins WAL on the
	// server until it is dropped.
	holdSlot sourceHold = iota
	// holdFence is the write fence of a run that blocked the table's
	// writes: every write is refused with SQLSTATE 55006 while it stands.
	holdFence
	// holdNone is a blocked-writes run that stopped before its fence went
	// on: the table kept taking writes and nothing stands on it.
	holdNone
)

// fenceAfterStop reports the hold a blocked-writes run that stopped short
// of done left on the source. It reads what the engine guarantees rather
// than the mode the run was asked for, so a run that failed before it
// fenced the table does not report the table as offline:
//
//   - a run abandoned on a data fault had its fence on, since the copy it
//     abandoned runs behind the fence; the abandon has taken it off again;
//   - a checkpoint row for the table means its fence stands: a fresh run
//     fences the source before it writes the row and takes the fence back
//     off when the row does not land, and a resumed run stands on a row
//     whose fence the stopped run left;
//   - no row means the run stopped before its fence went on.
//
// The read outlives the caller's context, since a cancelled run is a usual
// stop, but is bounded. When it fails, the hold is reported as the fence,
// the claim that sends the operator to look rather than one that tells
// them the table is untouched, and the read's error joins the run's.
func fenceAfterStop(ctx context.Context, store *checkpoint.Store, st statement.Statement, runErr error) (sourceHold, error) {
	var abandoned *schemachange.AbandonedError
	if errors.As(runErr, &abandoned) {
		return holdFence, runErr
	}
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbconn.DefaultConnectTimeout)
	defer cancel()
	_, err := store.Inspect(readCtx, st.Schema(), st.Table())
	switch {
	case err == nil:
		return holdFence, runErr
	case errors.Is(err, checkpoint.ErrNotFound), errors.Is(err, checkpoint.ErrTableMissing):
		return holdNone, runErr
	default:
		return holdFence, errors.Join(runErr, fmt.Errorf("read whether the stopped run fenced %s: %w", qualified(st), err))
	}
}

// checkBlockWritesAck confirms that the block-writes acknowledgement names the
// resolved target table, exactly as the force and restart acknowledgements
// must. A block-writes run refuses every write to the table for its whole
// length, so a typed table name is what stops the declaration from being
// left on a script and taking the next table offline by accident.
func checkBlockWritesAck(st statement.Statement, ack string) error {
	if ack == qualified(st) {
		return nil
	}
	return fmt.Errorf("the block-writes acknowledgement must name the resolved target table %q, got %q; nothing was executed",
		qualified(st), ack)
}

// auditBlockWrites records, before the fence goes on, that the operator took
// the table offline for writes. The record is warn-level and
// unconditional, as the force and restart audits are: an audit trail must
// not depend on diagnostics being enabled.
func auditBlockWrites(audit *slog.Logger, st statement.Statement) {
	audit.Warn("block-writes run acknowledged; every write to the table will be refused until the swap",
		"table", qualified(st),
		"sqlstate", schemachange.WriteFenceSQLState)
}
