package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// runDryRun is the imperative dry-run flow: the identical classify-and-route
// pipeline the declarative front-end uses, with the diff step skipped — the
// submitted statement feeds the classifier directly. It prints the routed
// plan and never executes anything. Introspecting the target table sharpens
// type-change classification; a missing table means zero facts and a
// strictly more conservative plan.
func (c *MigrateCmd) runDryRun(ctx context.Context, out io.Writer) error {
	logger := c.diag()
	st, err := statement.ParseOne(c.Alter)
	if err != nil {
		return err
	}
	logger.Debug("statement parsed", "kind", st.Kind, "schema", st.Schema, "table", st.Table)

	pool, err := dbconn.NewPool(ctx, c.Config())
	if err != nil {
		return err
	}
	defer pool.Close()

	facts, err := dryRunFacts(ctx, pool, st)
	if err != nil {
		return err
	}
	plan, err := planner.Classify(st.SQL, facts)
	if err != nil {
		return err
	}
	routed := router.Route([]planner.Plan{plan})
	logger.Debug("statement routed",
		"route", string(plan.Route), "disposition", string(routed.Disposition))

	if c.JSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(routed); err != nil {
			return fmt.Errorf("write dry-run plan: %w", err)
		}
		return nil
	}
	for _, rs := range routed.Statements {
		if err := writeChangeText(out, plannedFromRouted(rs)); err != nil {
			return err
		}
	}
	return nil
}

// dryRunFacts introspects the statement's target table for classifier
// facts. Statements without a single table target (index maintenance) and
// missing tables classify with zero facts.
func dryRunFacts(ctx context.Context, pool *pgxpool.Pool, st statement.Statement) (planner.Facts, error) {
	if st.Table == "" {
		return planner.Facts{}, nil
	}
	schema := st.Schema
	if schema == "" {
		schema = "public"
	}
	live, err := schemadiff.Introspect(ctx, pool, schema, st.Table)
	switch {
	case errors.Is(err, schemadiff.ErrTableNotFound):
		return planner.Facts{}, nil
	case err != nil:
		return planner.Facts{}, err
	}
	return liveFacts(live), nil
}

// plannedFromRouted adapts a routed statement to the shared text renderer.
func plannedFromRouted(rs router.Statement) plannedChange {
	return plannedChange{
		Change:      schemadiff.Change{SQL: rs.Statement},
		Route:       rs.Route,
		Backend:     rs.Backend,
		Disposition: rs.Disposition,
		Decisions:   rs.Decisions,
		ExecSQL:     rs.ExecSQL,
	}
}
