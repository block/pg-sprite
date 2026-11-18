package migrate

import (
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// CopySwapMinConns is the smallest pool the copy-and-swap route admits for
// a copy sized by o: schemachange.MinPoolConns, the size the Runner itself
// holds every run to, named here so a host that builds its pool for Run or
// RunDesired (dbconn.Config.MaxConnsFloor) sizes it from the package it
// calls. Run and RunDesired refuse a smaller pool before the preflight.
func CopySwapMinConns(o copier.Options) int32 {
	return schemachange.MinPoolConns(o)
}

// copySwapPoolVerdict is Run's failed verdict for a pool the route refused
// before it read or wrote anything. The detail names the remedy as a
// concept; how a front door sizes its pool is the front door's to spell.
func copySwapPoolVerdict(st statement.Statement, err error) verdict.Verdict {
	return verdict.Verdict{
		Outcome:   verdict.OutcomeFailed,
		Code:      string(executor.CodePoolTooSmall),
		Statement: st.SQL(),
		Table:     qualified(st),
		Detail: "the copy-and-swap route refused the pool: " + err.Error() +
			"; nothing was created. Give the pool more connections, or copy with fewer workers",
	}
}
