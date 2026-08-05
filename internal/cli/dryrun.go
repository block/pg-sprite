package cli

import (
	"context"
	"errors"
	"io"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/plan"
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
	classified, err := planner.Classify(st.SQL, facts)
	if err != nil {
		return err
	}
	routed := router.Route([]planner.Plan{classified})
	logger.Debug("statement routed",
		"route", string(classified.Route), "disposition", string(routed.Disposition))

	report := plan.NewReport(plan.SourceAlter)
	report.Schema = st.Schema
	report.Table = st.Table
	report.Disposition = routed.Disposition
	for _, rs := range routed.Statements {
		report.Statements = append(report.Statements, plan.FromRouted(rs))
	}

	if c.JSON {
		return writeJSON(out, report)
	}
	for _, ps := range report.Statements {
		if err := writeChangeText(out, ps); err != nil {
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
