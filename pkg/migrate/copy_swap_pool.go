package migrate

import (
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// copySwapReservedConns is how many pool connections the copy-and-swap
// route keeps beside its copy workers: one for the catch-up's flushes and
// one for the checkpoint saves and progress reads. A quiesced run has no
// catch-up, but is held to the same size so one pool serves either run.
const copySwapReservedConns = 2

// CopySwapMinConns is the smallest pool the copy-and-swap route admits for
// a copy sized by o: one connection per copy worker, with zero workers
// meaning copier.DefaultWorkers, and two beside them. Each worker holds a
// pool connection while it copies a chunk, so a pool of only the workers
// leaves a live run's catch-up flushes waiting behind the copy while the
// slot's lag grows. A host that builds its own pool sizes it with this
// (dbconn.Config.MaxConnsFloor); Run and RunDesired refuse a smaller one.
func CopySwapMinConns(o copier.Options) int32 {
	workers := o.Workers
	if workers == 0 {
		workers = copier.DefaultWorkers
	}
	return int32(workers) + copySwapReservedConns
}

// checkCopySwapPool refuses a pool smaller than CopySwapMinConns for the
// route's copy options, wrapping executor.ErrPoolTooSmall, so the verdict
// carries the pool-too-small code the native route's admission uses.
func checkCopySwapPool(pool *pgxpool.Pool, opts CopySwapOptions) error {
	need := CopySwapMinConns(opts.Copier)
	have := pool.Config().MaxConns
	if have >= need {
		return nil
	}
	return fmt.Errorf("copy-and-swap needs %d connections (%d copy workers, one for the catch-up's flushes, one for checkpoint saves and progress reads), pool holds %d: %w",
		need, need-copySwapReservedConns, have, executor.ErrPoolTooSmall)
}

// copySwapPoolVerdict is Run's failed verdict for a pool the route refused
// before it read or wrote anything.
func copySwapPoolVerdict(st statement.Statement, err error) verdict.Verdict {
	return verdict.Verdict{
		Outcome:   verdict.OutcomeFailed,
		Code:      string(executor.CodePoolTooSmall),
		Statement: st.SQL(),
		Table:     qualified(st),
		Detail: "the copy-and-swap route refused the pool: " + err.Error() +
			"; nothing was created. Raise pool_max_conns in the connection URL (or dbconn.Config.MaxConns), or run fewer copy workers",
	}
}
