package preflight

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// CheckCopySwap runs the copy-and-swap route's three admission checks in
// order — CheckPrivileges at TierCopyAndSwap (with replication access when
// the run decodes WAL), CheckCopySwapShape, CheckCopySwapEnvironment — and
// returns the CopySwapTarget the last one mints. It is the one call a
// caller needs before the first write; the three checks stay exported for
// callers that report each refusal class separately.
func CheckCopySwap(ctx context.Context, pool *pgxpool.Pool, schema, table string, env CopySwapEnvironment) (CopySwapTarget, error) {
	role, err := CheckPrivileges(ctx, pool, schema, table, Requirement{Tier: TierCopyAndSwap, LogicalDecoding: env.LogicalDecoding})
	if err != nil {
		return CopySwapTarget{}, err
	}
	shape, err := CheckCopySwapShape(ctx, pool, schema, table, role)
	if err != nil {
		return CopySwapTarget{}, err
	}
	return CheckCopySwapEnvironment(ctx, pool, shape, env)
}
