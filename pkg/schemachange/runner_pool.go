package schemachange

import (
	"fmt"
	"math"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/executor"
)

// reservedPoolConns is how many pool connections a run keeps beside its
// copy workers: one for the catch-up's flushes and one for the checkpoint
// saves and progress reads. A quiesced run has no catch-up, but is held to
// the same size so one pool serves either run.
const reservedPoolConns = 2

// MinPoolConns is the smallest pool Run admits for a copy sized by o: one
// connection per copy worker, with zero workers meaning
// copier.DefaultWorkers, and two beside them. Each worker holds a pool
// connection while it copies a chunk, so a pool of only the workers would
// leave a live run's catch-up flushes waiting behind the copy while the
// slot's lag grows. A host that builds its own pool sizes it with this
// (dbconn.Config.MaxConnsFloor). A worker count no pool size can hold
// answers math.MaxInt32 rather than wrapping to a number every pool would
// pass.
func MinPoolConns(o copier.Options) int32 {
	workers := o.Workers
	if workers == 0 {
		workers = copier.DefaultWorkers
	}
	if workers > math.MaxInt32-reservedPoolConns {
		return math.MaxInt32
	}
	return int32(workers) + reservedPoolConns
}

// CheckPool refuses a pool smaller than MinPoolConns(o) with an error
// wrapping executor.ErrPoolTooSmall, the refusal the native route's
// admission gives, so a caller classifies both with one errors.Is. Run
// calls it before it takes the table lock; a front door that refuses
// earlier calls it too.
func CheckPool(pool *pgxpool.Pool, o copier.Options) error {
	need := MinPoolConns(o)
	have := pool.Config().MaxConns
	if have >= need {
		return nil
	}
	return fmt.Errorf("copy-and-swap needs %d connections (%d copy workers, one for the catch-up's flushes, one for checkpoint saves and progress reads), pool holds %d: %w",
		need, need-reservedPoolConns, have, executor.ErrPoolTooSmall)
}
