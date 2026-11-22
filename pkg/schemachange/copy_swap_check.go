package schemachange

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/preflight"
)

// CheckCopySwap is the copy-and-swap route's preflight for a run that may
// resume a stopped one: the privilege, shape, and environment checks
// preflight.CheckCopySwap folds, with env.Held filled from the server by
// MeasureResumeHeld between the shape and the environment check, so a
// resume is asked only for the disk left to grow. Whatever Held the caller
// set is replaced.
//
// restart is the run's RunOptions.Restart: a run that starts over is
// checked as a fresh run, with Held zero and nothing measured. Its discard
// has not yet freed what the stopped run holds when the preflight runs, so
// counting that space would ask the run for less than it will need.
func CheckCopySwap(ctx context.Context, pool *pgxpool.Pool, store *checkpoint.Store, schema, table string, env preflight.CopySwapEnvironment, restart bool) (preflight.CopySwapTarget, error) {
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
	if !restart {
		if env.Held, err = MeasureResumeHeld(ctx, pool, store, shape, env.LogicalDecoding); err != nil {
			return preflight.CopySwapTarget{}, err
		}
	}
	return preflight.CheckCopySwapEnvironment(ctx, pool, shape, env)
}
