package schemachange

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/preflight"
)

// CopySwapStart says how the run CheckCopySwap checks meets a stopped run
// on the same table. Its zero value is neither way and is refused, so a
// caller always says which it means.
type CopySwapStart int

const (
	// CopySwapResume is a run that resumes whatever stopped run the
	// table's checkpoint row records, or starts fresh when there is none:
	// the preflight asks it only for the disk left to grow.
	CopySwapResume CopySwapStart = iota + 1
	// CopySwapRestart is a run that starts over (RunOptions.Restart): the
	// preflight checks it as a fresh run and measures nothing.
	CopySwapRestart
)

// CheckCopySwap is the copy-and-swap route's preflight for a run that may
// resume a stopped one: the privilege, shape, and environment checks
// preflight.CheckCopySwap folds, with env.Held filled from the server by
// MeasureResumeHeld between the shape and the environment check, so a
// resume is asked only for the disk left to grow. Whatever Held the caller
// set is replaced. preflight.CheckCopySwap discounts nothing, so it asks a
// resume for the whole requirement again.
//
// start is CopySwapRestart for a run that starts over: it is checked as a
// fresh run, with Held zero and nothing measured. Its discard has not yet
// freed what the stopped run holds when the preflight runs, so counting
// that space would ask the run for less than it will need. Any other run
// is CopySwapResume.
//
// The preflight writes nothing, so it is safe to call before the run's
// first write (ST-6); a refusal or a failed read leaves the database as it
// was. It takes no lock on the table, but the resume measure's read of the
// shadow's size locks the shadow, so it waits out a concurrent DDL on it
// until ctx ends. store is the checkpoint store the run will use, on the
// same database as pool; it is read only for CopySwapResume, and nil is
// refused there. The free disk in env is a reading the caller takes, and
// Held is space that reading already excludes, so measure it right before
// the call: a target checked at plan time is a forecast, and the apply
// calls again.
func CheckCopySwap(ctx context.Context, pool *pgxpool.Pool, store *checkpoint.Store, schema, table string, env preflight.CopySwapEnvironment, start CopySwapStart) (preflight.CopySwapTarget, error) {
	if err := checkCopySwapStart(store, start); err != nil {
		return preflight.CopySwapTarget{}, err
	}
	role, err := preflight.CheckPrivileges(ctx, pool, schema, table,
		preflight.Requirement{Tier: preflight.TierCopyAndSwap, LogicalDecoding: env.LogicalDecoding})
	if err != nil {
		return preflight.CopySwapTarget{}, err
	}
	shape, err := preflight.CheckCopySwapShape(ctx, pool, schema, table, role)
	if err != nil {
		return preflight.CopySwapTarget{}, err
	}
	env.Held = preflight.CopySwapHeld{}
	if start == CopySwapResume {
		if env.Held, err = MeasureResumeHeld(ctx, pool, store, shape, env.LogicalDecoding); err != nil {
			return preflight.CopySwapTarget{}, err
		}
	}
	return preflight.CheckCopySwapEnvironment(ctx, pool, shape, env)
}

// checkCopySwapStart refuses a start that is neither way, and a resume
// with no store to read the stopped run's row from.
func checkCopySwapStart(store *checkpoint.Store, start CopySwapStart) error {
	switch start {
	case CopySwapResume:
		if store == nil {
			return fmt.Errorf("%w: a copy-and-swap preflight for a resume needs the checkpoint store", ErrInvalidOptions)
		}
		return nil
	case CopySwapRestart:
		return nil
	default:
		return fmt.Errorf("%w: copy-and-swap start %d is neither CopySwapResume nor CopySwapRestart", ErrInvalidOptions, start)
	}
}
