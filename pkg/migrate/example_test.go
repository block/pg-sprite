package migrate_test

import (
	"context"
	"fmt"
	"log"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// Example_run is the full library flow an orchestrator embeds: parse the
// statement, gate it before dialing, connect, and drive the imperative
// pipeline to exactly one verdict. It is compile-checked but not executed —
// Run needs a live PostgreSQL database.
func Example_run() {
	ctx := context.Background()

	// Parse failures surface here, at the boundary where the embedder can
	// render them.
	st, err := statement.ParseOne("ALTER TABLE users ALTER COLUMN email SET NOT NULL")
	if err != nil {
		log.Print(err)
		return
	}

	// Gate needs no database: an unsupported statement kind refuses before
	// dialing. Run re-checks it, so skipping this early gate is safe —
	// only slower.
	if v, refused := migrate.Gate(st); refused {
		fmt.Println(v.Reason, v.Detail)
		return
	}

	pool, err := dbconn.NewPool(ctx, dbconn.Config{URL: "postgres://engine@localhost:5432/app"})
	if err != nil {
		log.Print(err)
		return
	}
	defer pool.Close()

	// The zero Options is not a runnable policy — Run rejects it.
	// DefaultOptions is the sanctioned starting point (the CLI's flag
	// defaults); tune it per table: a large table needs more generous
	// concurrent-build and validate bounds, a hot table a tighter lock
	// budget.
	opts := migrate.DefaultOptions()

	// The verdict-and-error contract has three shapes: a refusal returns
	// the verdict with a nil error; an execution failure returns the
	// failed verdict (the stable code and the committed prefix) together
	// with the operational error; an error with a zero verdict means the
	// pipeline stopped before executing anything.
	v, err := migrate.Run(ctx, pool, st, opts)
	if err != nil {
		log.Print(err)
	}
	switch v.Outcome {
	case verdict.OutcomeExecuted:
		fmt.Println(v.ExecutedSQL)
	case verdict.OutcomeRefused:
		fmt.Println(v.Reason, v.Detail)
	case verdict.OutcomeFailed:
		fmt.Println(v.Code, v.ExecutedSQL)
	}
}
