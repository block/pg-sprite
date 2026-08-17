package cli

import (
	"context"
	"errors"
	"io"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// runDryRun is the imperative dry-run flow: the identical classify-and-route
// pipeline the declarative front-end uses, with the diff step skipped — the
// submitted statement feeds the classifier directly. It prints the routed
// plan and never executes anything. The statement-type gate runs first,
// exactly as it does on apply, so a gated kind dry-runs to the same refusal
// verdict (and refusal exit code) the apply would end in. Introspecting the
// target table sharpens type-change classification; a missing table means
// zero facts and a strictly more conservative plan.
func (c *MigrateCmd) runDryRun(ctx context.Context, out io.Writer) error {
	logger := c.diag()
	st, err := statement.ParseOne(c.Alter)
	if err != nil {
		return err
	}
	logger.Debug("statement parsed", "kind", st.Kind(), "schema", st.Schema(), "table", st.Table())
	if v, refused := gateVerdict(st); refused {
		return c.emit(out, v)
	}

	pool, err := dbconn.NewPool(ctx, c.Config())
	if err != nil {
		return err
	}
	defer pool.Close()

	facts, targetFacts, tableExists, err := dryRunFacts(ctx, pool, st)
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
	report.TableExists = tableExists
	if report.ServerVersion, err = dbconn.ServerVersion(ctx, pool); err != nil {
		return err
	}
	report.Disposition = routed.Disposition
	for _, rs := range routed.Statements {
		report.Statements = append(report.Statements, plan.FromRouted(rs))
	}
	if targetFacts.Partitioned() {
		refused := make([]bool, len(report.Statements))
		for i := range report.Statements {
			var cause preflight.PartitionRefusalCause
			cause, err = preflight.RefusesPartitionedParent(targetFacts.ServerMajor(), report.Statements[i].ExecSQL)
			if err != nil {
				return err
			}
			refused[i] = cause != ""
		}
		plan.RefuseUnsupportedPartitionedParent(&report, refused)
	}
	report.Fingerprint = plan.Fingerprint(report.Statements)

	if c.JSON {
		if err := writeJSON(out, report); err != nil {
			return err
		}
	} else if err := writeDryRunText(out, report); err != nil {
		return err
	}
	// A plan execution would not run exits with the refusal code — the same
	// contract migrate uses for refusal verdicts — so CI can gate on the
	// dry run without parsing the report. A missing target table carries
	// the same code: the plan was classified from zero facts and running
	// without --dry-run would fail, so a gate must not read it as green.
	if report.Disposition != router.DispositionExecute || tableMissing(report) {
		return verdict.ErrRefused
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
// facts. Statements without a single table target (index drops, REINDEX)
// and missing tables classify with zero facts. The returned tableExists
// mirrors the introspection outcome for the report: true when the table
// was found, false when it was looked up and missing, nil when the
// statement has no single table target to introspect.
func dryRunFacts(ctx context.Context, pool *pgxpool.Pool, st statement.Statement) (planner.Facts, preflight.TargetFacts, *bool, error) {
	if st.Table() == "" {
		return planner.Facts{}, preflight.TargetFacts{}, nil, nil
	}
	live, err := schemadiff.Introspect(ctx, pool, resolvedSchema(st), st.Table())
	switch {
	case errors.Is(err, schemadiff.ErrTableNotFound):
		exists := false
		return planner.Facts{}, preflight.TargetFacts{}, &exists, nil
	case err != nil:
		return planner.Facts{}, preflight.TargetFacts{}, nil, err
	}
	targetFacts, err := preflight.LookupTargetFacts(ctx, pool, resolvedSchema(st), st.Table())
	if err != nil {
		return planner.Facts{}, preflight.TargetFacts{}, nil, err
	}
	exists := true
	return planner.FactsFrom(live), targetFacts, &exists, nil
}
