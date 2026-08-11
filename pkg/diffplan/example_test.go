package diffplan_test

import (
	"context"
	"fmt"
	"log"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/statement"
)

// Example_plan is the full library flow an orchestrator embeds: parse the
// desired-state schema, connect, and derive the routed convergence plan.
// It is compile-checked but not executed — Plan needs a live PostgreSQL
// database.
func Example_plan() {
	ctx := context.Background()

	// Parse refusals (inadmissible desired files) surface here, at the
	// boundary where the embedder can render them.
	ds, err := statement.ParseDesired(
		"CREATE TABLE events (id bigint PRIMARY KEY, name varchar(50) NOT NULL);\n" +
			"CREATE INDEX events_name_idx ON events (name);")
	if err != nil {
		log.Print(err)
		return
	}

	// Plan is not read-only: the desired DDL runs in an always-rolled-back
	// scratch schema, so connect read-write (not a hot standby) as a role
	// with CREATE on the target database. The live table is never written.
	pool, err := dbconn.NewPool(ctx, dbconn.Config{URL: "postgres://engine@localhost:5432/app"})
	if err != nil {
		log.Print(err)
		return
	}
	defer pool.Close()

	report, err := diffplan.Plan(ctx, pool, diffplan.Request{Schema: "public", Desired: ds})
	if err != nil {
		log.Print(err)
		return
	}

	// Disposition says whether the whole plan can execute; each statement
	// carries its route and the engine's canonical SQL rendering.
	fmt.Println(report.Disposition)
	for _, st := range report.Statements {
		fmt.Println(st.Route, st.SQL)
	}
}
