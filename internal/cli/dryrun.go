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
// verdict (and refusal exit code) the apply would end in; so does the
// destructive-change gate, unless --allow-destructive names the table, and
// the executor's shape admission of the routed steps, once they are known, when
// no earlier apply check refuses first. The checks that need the apply's
// live facts — the size guard and the privilege preflight, which the apply
// runs before admission — are not run. Introspecting the
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

	facts, err := migrate.LiveFacts(ctx, pool, st)
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
	classified, err := planner.Classify(canonical, facts.Classifier)
	if err != nil {
		return err
	}
	routed := router.Route([]planner.Plan{classified})
	logger.Debug("statement routed",
		"route", string(classified.Route), "disposition", string(routed.Disposition))
	// The apply refuses a destructive statement before any route runs it,
	// so without the acknowledgement the dry run ends in that same refusal
	// verdict; with it, the plan below is the one the acknowledged apply
	// takes.
	v, refused, err := migrate.AdmitDestructive(st, facts.Schema, routed.Statements[0], c.AllowDestructive)
	if err != nil {
		return err
	}
	if refused {
		return c.emit(out, v)
	}

	report := plan.NewReport(plan.SourceAlter)
	report.Schema = facts.Schema
	report.Table = st.Table()
	report.TableExists = facts.TableExists
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
	if facts.Target.Partitioned() {
		causes := make([]preflight.PartitionRefusalCause, len(report.Statements))
		for i := range report.Statements {
			causes[i], err = preflight.RefusesPartitionedParent(facts.Target.ServerMajor(), report.Statements[i].ExecSQL)
			if err != nil {
				return err
			}
		}
		if err = plan.RefuseUnsupportedPartitionedParent(&report, causes); err != nil {
			return err
		}
	}
	// A refusal the apply decides from the routed steps' shape alone is
	// knowable now, so the dry run ends in that same refusal
	// verdict instead of an executable plan. The checks that need live
	// facts (size guard, privileges) stay the apply's; a missing target
	// table is reported as such, since the apply stops there first.
	if !tableMissing(report) {
		for _, ps := range report.Statements {
			if ps.Disposition != router.DispositionExecute {
				continue
			}
			// Only the native route's steps run through the sequence
			// executor; a copy-and-swap statement has none, since the
			// engine builds its shadow from the statement itself.
			if ps.Route != planner.RouteNative {
				continue
			}
			v, refused, admitErr := migrate.AdmitRoutedSteps(st, report.Schema, ps.ExecSQL)
			if admitErr != nil {
				return admitErr
			}
			if refused {
				return c.emit(out, v)
			}
		}
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
