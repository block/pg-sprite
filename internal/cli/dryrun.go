package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/router"
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
	if v, refused := migrate.Gate(st); refused {
		return c.emit(out, v)
	}

	pool, err := dbconn.NewPool(ctx, c.Config())
	if err != nil {
		return err
	}
	defer pool.Close()

	facts, targetFacts, tableExists, err := migrate.LiveFacts(ctx, pool, st)
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
	report.Schema = migrate.ResolvedSchema(st)
	report.Table = st.Table()
	report.TableExists = tableExists
	if report.ServerVersion, err = dbconn.ServerVersion(ctx, pool); err != nil {
		return err
	}
	report.Disposition = routed.Disposition
	for _, rs := range routed.Statements {
		ps, err := plan.FromRouted(rs)
		if err != nil {
			return fmt.Errorf("plan statement %q: %w", rs.Statement, err)
		}
		report.Statements = append(report.Statements, ps)
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
	} else if err := writeDryRunText(out, c.palette(out), report); err != nil {
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
