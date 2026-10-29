package schemachange

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
)

// AbandonedError is the error Runner.Run returns for a run it abandoned on
// a data fault: the server rejected the rows as the statement would shape
// them (IsDataFault), so the same run would stop on the same rows however
// often it was resumed, and the Runner discarded it — the shadow, the hold
// on the source, the checkpoint row — under its own lock session before
// returning. Nothing of the run remains; the next Run of the statement
// starts from the beginning. Fault is the server's error, and Unwrap
// exposes it so errors.As against *pgconn.PgError still reads the
// SQLSTATE.
type AbandonedError struct {
	// Schema and Table name the target whose run was abandoned.
	Schema, Table string
	// Fault is the data fault the run stopped on.
	Fault error
}

func (e *AbandonedError) Error() string {
	return fmt.Sprintf("copy-and-swap run on %s.%s abandoned on a data fault: %v", e.Schema, e.Table, e.Fault)
}

func (e *AbandonedError) Unwrap() error { return e.Fault }

// abandonOnDataFault decides what a run that stopped in its fill or swap
// with err leaves behind. A refusal, a lost lock, and every stop that is
// not a data fault are returned as they came: the row stays at its last
// save and the next Run resumes it. A data fault is abandoned: the run is
// discarded as a restart would discard it, and the result is an
// *AbandonedError carrying the fault. A discard that fails is joined
// behind the fault, so the caller reads both and the row — still standing
// as far as the discard got — is a restart's to finish.
func (r *Runner) abandonOnDataFault(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, target preflight.CopySwapTarget, state *runState, err error) error {
	if !abandons(lock, err) {
		return err
	}
	if derr := r.discardRow(ctx, pool, lock, target, state.cp); derr != nil {
		return errors.Join(err, fmt.Errorf("abandon run on %s.%s: %w", state.cp.Schema, state.cp.Table, derr))
	}
	return &AbandonedError{Schema: state.cp.Schema, Table: state.cp.Table, Fault: err}
}

// abandons reports whether a run that stopped with err is one to abandon:
// a data fault reached with the lock session still held and no refusal
// riding on the error. A refusal is a decision the caller branches on and
// is never masked by a discard; a lost lock cannot discard anything.
func abandons(lock *dbconn.TableLockSession, err error) bool {
	if lock.Err() != nil {
		return false
	}
	if RefusalCauseOf(err) != "" {
		return false
	}
	return IsDataFault(err)
}
