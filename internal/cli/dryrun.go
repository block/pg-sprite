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
	logger.Debug("statement parsed", "kind", st.Kind(), "schema", st.Schema(), "table", st.Table())

	pool, err := dbconn.NewPool(ctx, c.Config())
	if err != nil {
		return err
	}
	defer pool.Close()

	facts, err := dryRunFacts(ctx, pool, st)
	if err != nil {
		return err
	}
	// The report carries the engine's canonical rendering, not an echo of
	// the submitted text, so both front doors describe the same change
	// with the same string (and the fingerprint agrees across them).
	canonical, err := statement.Canonical(st.SQL())
	if err != nil {
		return err
	}
	classified, err := planner.Classify(canonical, facts)
	if err != nil {
		return err
	}
	routed := router.Route([]planner.Plan{classified})
	logger.Debug("statement routed",
		"route", string(classified.Route), "disposition", string(routed.Disposition))

	report := plan.NewReport(plan.SourceAlter)
	report.Schema = resolvedSchema(st)
	report.Table = st.Table()
	if report.ServerVersion, err = dbconn.ServerVersion(ctx, pool); err != nil {
		return err
	}
	report.Disposition = routed.Disposition
	for _, rs := range routed.Statements {
		report.Statements = append(report.Statements, plan.FromRouted(rs))
	}
	report.Fingerprint = plan.Fingerprint(report.Statements)

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

// resolvedSchema is the schema the engine plans against: the statement's
// qualification, or public — the default the engine introspects — when a
// table-targeted statement leaves it unqualified. The report carries the
// resolved name, never the submitted one: a stored plan must not depend on
// the reader's search_path to say which table it describes.
func resolvedSchema(st statement.Statement) string {
	if st.Schema() == "" && st.Table() != "" {
		return "public"
	}
	return st.Schema()
}

// dryRunFacts introspects the statement's target table for classifier
// facts. Statements without a single table target (index maintenance) and
// missing tables classify with zero facts.
func dryRunFacts(ctx context.Context, pool *pgxpool.Pool, st statement.Statement) (planner.Facts, error) {
	if st.Table() == "" {
		return planner.Facts{}, nil
	}
	live, err := schemadiff.Introspect(ctx, pool, resolvedSchema(st), st.Table())
	switch {
	case errors.Is(err, schemadiff.ErrTableNotFound):
		return planner.Facts{}, nil
	case err != nil:
		return planner.Facts{}, err
	}
	return planner.FactsFrom(live), nil
}
