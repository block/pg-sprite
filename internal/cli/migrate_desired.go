package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

func (c *MigrateCmd) validateInput() error {
	if (c.Alter == "") == (c.Desired == "") {
		return errors.New("provide exactly one of --alter or --desired")
	}
	if c.Desired == "" && c.ExpectFingerprint != "" {
		return errors.New("--expect-fingerprint requires --desired: it pins a desired-state plan, and --alter runs one statement with no plan to pin")
	}
	if c.Desired != "" {
		if c.Force != "" {
			return errors.New("--force cannot be combined with --desired")
		}
		if c.AcceptBlocking != "" {
			return errors.New("--accept-blocking cannot be combined with --desired")
		}
		if c.Restart != "" {
			return errors.New("--restart cannot be combined with --desired: the plan, not the operator, decides which run the acknowledgement would discard; run the stopped statement with migrate --alter --copy-and-swap --restart instead")
		}
		if c.AllowDestructive != "" && c.ExpectFingerprint == "" {
			return errors.New("--allow-destructive with --desired requires --expect-fingerprint: a destructive plan runs only when the run pins the reviewed plan; review it with diff --desired, then pass its fingerprint")
		}
		if c.BlockWrites != "" {
			return errors.New("--block-writes cannot be combined with --desired: the plan, not the operator, decides how many copies would block the table's writes; run each statement with migrate --alter --block-writes instead")
		}
	}
	return nil
}

// desiredOptions is the run policy for a desired-state run. Its destructive
// acknowledgement travels on the request, bound to the pinned plan, never in
// the policy, which desired-state execution rejects it from.
func (c *MigrateCmd) desiredOptions(logger *slog.Logger) migrate.Options {
	opts := c.options(logger)
	opts.AllowDestructive = ""
	return opts
}

// previewDesiredPlan returns the plan a desired-state dry run prints. A plan
// that discards live structure is previewed as refused, as a run refuses it,
// unless the dry run carries the pin and the acknowledgement the run needs.
// For a refusal it also returns the fingerprint a run must pin, which is not
// the refused preview's own.
func (c *MigrateCmd) previewDesiredPlan(report plan.Report) (preview plan.Report, pinnable string) {
	if !hasDestructive(report) {
		return report, ""
	}
	if c.liftsDestructiveRefusal(report) {
		return report, ""
	}
	plan.RefuseDestructive(&report)
	return report, report.PinnableFingerprint
}

// writeDesiredPreview prints a desired-state dry run's plan and, for a
// refused destructive plan, the flags that lift the refusal. It checks the
// acknowledged table first and the pin next, as the run does, before
// anything is rendered. A plan the flags cannot lift gets the refusal a
// pinned run gives it: as the result when the dry run carries the flags,
// and in place of the flags when it does not.
func (c *MigrateCmd) writeDesiredPreview(out io.Writer, report plan.Report) error {
	if err := migrate.CheckDesiredAllowDestructiveTable(c.AllowDestructive, report); err != nil {
		return err
	}
	if stale, ok := migrate.CheckPlanPin(c.ExpectFingerprint, report); !ok {
		return c.finishDesired(out, stale, nil)
	}
	if c.liftsDestructiveRefusal(report) {
		if refusal, ok := refusedEvenWhenPinned(report); !ok {
			return c.finishDesired(out, refusal, nil)
		}
	}
	preview, pinnable := c.previewDesiredPlan(report)
	diff := DiffCmd{DBFlags: c.DBFlags, OutputFlags: c.OutputFlags, Schema: c.Schema, ExtensionSchema: c.ExtensionSchema, JSON: c.JSON}
	reportErr := diff.writeDiffReport(out, preview)
	if pinnable == "" || c.JSON {
		return reportErr
	}
	w := &stickyWriter{out: out, pal: c.palette(out), started: true}
	if refusal, ok := refusedEvenWhenPinned(report); !ok {
		w.diag("note", "", "pinning this plan does not let it run: "+refusal.Detail)
		if w.err != nil {
			return w.err
		}
		return reportErr
	}
	w.diag("note", "", "a run refuses a plan that discards live structure unless it pins the reviewed plan and names the table; to run this plan, add to the command:")
	w.printf("  --expect-fingerprint %s --allow-destructive %s.%s\n", pinnable, report.Schema, report.Table)
	w.diag("note", "", "with both flags this dry run previews the plan the run would execute")
	if w.err != nil {
		return w.err
	}
	return reportErr
}

// hasDestructive reports whether the plan discards any live structure.
func hasDestructive(report plan.Report) bool {
	return slices.ContainsFunc(report.Statements, func(ps plan.Statement) bool { return ps.Destructive })
}

// liftsDestructiveRefusal reports whether the dry run carries the pin and the
// acknowledgement a run needs for the plan's destructive statements.
func (c *MigrateCmd) liftsDestructiveRefusal(report plan.Report) bool {
	if !hasDestructive(report) {
		return false
	}
	return c.AllowDestructive == report.Schema+"."+report.Table && c.ExpectFingerprint == report.Fingerprint
}

// refusedEvenWhenPinned reports the refusal a run gives a destructive plan
// that the pin and the acknowledgement cannot lift, such as a drop beside a
// copy-and-swap statement, a partitioned parent's index drop, or a statement
// with no safe path. The dry run prints that refusal, as the run would,
// instead of the flags, which would only lead to it.
func refusedEvenWhenPinned(report plan.Report) (migrate.DesiredResult, bool) {
	if !hasDestructive(report) {
		return migrate.DesiredResult{}, true
	}
	res, ok := migrate.AdmitPlan(migrate.DesiredRequest{
		ExpectedFingerprint: report.Fingerprint,
		AllowDestructive:    report.Schema + "." + report.Table,
	}, report)
	return res, ok
}

func (c *MigrateCmd) runDesired(ctx context.Context, out io.Writer) error {
	raw, err := os.ReadFile(c.Desired)
	if err != nil {
		return fmt.Errorf("read desired schema: %w", err)
	}
	file, err := statement.ParseDesiredFile(string(raw))
	if err != nil {
		return desiredFileRefusal(out, c.palette(out), c.JSON, false, err, "")
	}
	if enumType, ok := file.EnumType(); ok {
		return c.runDesiredEnumType(ctx, out, enumType)
	}
	if rowSecurity, ok := file.RowSecurity(); ok {
		if rowSecurity, err = rowSecurity.WithExtensionSchemas(c.ExtensionSchema...); err != nil {
			return fmt.Errorf("--extension-schema: %w", err)
		}
		return c.runDesiredRowSecurity(ctx, out, rowSecurity)
	}
	desired, ok := file.Schema()
	if !ok {
		return fmt.Errorf("desired schema %s admitted no planner input", c.Desired)
	}
	if desired, err = desired.WithExtensionSchemas(c.ExtensionSchema...); err != nil {
		return fmt.Errorf("--extension-schema: %w", err)
	}
	pool, err := dbconn.NewPool(ctx, c.Config())
	if err != nil {
		return err
	}
	defer pool.Close()
	if c.DryRun {
		report, err := diffplan.Plan(ctx, pool, diffplan.Request{Schema: c.Schema, Desired: desired})
		if err != nil {
			return desiredFileRefusal(out, c.palette(out), c.JSON, false, err, desired.Statements()[0].SQL())
		}
		// Preview uses desired-state admission: a routed native step can still
		// discard live structure, which RunDesired refuses before execution
		// unless the run pins the plan and names the table.
		return c.writeDesiredPreview(out, report)
	}
	opts, stopProgress, err := c.startProgress(ctx, c.desiredOptions(c.diag()))
	if err != nil {
		return err
	}
	result, runErr := migrate.RunDesired(ctx, pool, migrate.DesiredRequest{
		Schema:              c.Schema,
		Desired:             desired,
		ExpectedFingerprint: c.ExpectFingerprint,
		AllowDestructive:    c.AllowDestructive,
	}, opts)
	stopProgress()
	return c.finishDesired(out, result, runErr)
}

// runDesiredEnumType converges a desired enum type file: a dry run prints
// the plan diff prints, and a run adds each label the live type lacks. As
// for diff, --extension-schema has no effect on an enum type file.
func (c *MigrateCmd) runDesiredEnumType(ctx context.Context, out io.Writer, desired statement.DesiredEnumType) error {
	if c.AllowDestructive != "" {
		return errors.New("--allow-destructive does not apply to an enum type file: a run only adds labels, and no plan drops one")
	}
	pool, err := dbconn.NewPool(ctx, c.Config())
	if err != nil {
		return err
	}
	defer pool.Close()
	if c.DryRun {
		diff := DiffCmd{DBFlags: c.DBFlags, OutputFlags: c.OutputFlags, Schema: c.Schema, JSON: c.JSON}
		report, err := diffplan.PlanEnumType(ctx, pool, c.Schema, desired)
		if err != nil {
			return desiredFileRefusal(out, c.palette(out), c.JSON, false, err, desired.Statements()[0].SQL())
		}
		if stale, ok := migrate.CheckPlanPin(c.ExpectFingerprint, report); !ok {
			return c.finishDesired(out, stale, nil)
		}
		return diff.writeDiffReport(out, report)
	}
	opts, stopProgress, err := c.startProgress(ctx, c.desiredOptions(c.diag()))
	if err != nil {
		return err
	}
	result, runErr := migrate.RunDesiredEnumType(ctx, pool, migrate.EnumTypeRequest{
		Schema: c.Schema, Desired: desired, ExpectedFingerprint: c.ExpectFingerprint,
	}, opts)
	stopProgress()
	return c.finishDesired(out, result, runErr)
}

// finishDesired writes a desired-state result and returns the exit error
// its outcome maps to.
func (c *MigrateCmd) finishDesired(out io.Writer, result migrate.DesiredResult, runErr error) error {
	if result.Outcome == "" {
		return runErr
	}
	if err := c.writeDesiredResult(out, result); err != nil {
		return err
	}
	if runErr != nil {
		return runErr
	}
	if result.Outcome == verdict.OutcomeRefused {
		return verdict.ErrRefused
	}
	return nil
}

func (c *MigrateCmd) writeDesiredResult(out io.Writer, result migrate.DesiredResult) error {
	if c.JSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(result); err != nil {
			return fmt.Errorf("write desired result: %w", err)
		}
		return nil
	}
	pal := c.palette(out)
	for _, v := range result.Verdicts {
		if err := writeVerdictText(out, pal, v); err != nil {
			return err
		}
	}
	return writeDesiredSummary(out, pal, result)
}

// writeDesiredSummary writes the run's closing line. A whole-run refusal
// (a file refused before anything planned) gets the verdict layout's
// headline, class, owner, and cause, because it carries no statement verdict
// that would show them. A refusal partway through a run already printed
// them in its statement's verdict, so it and every other outcome get one
// line.
func writeDesiredSummary(out io.Writer, pal palette, result migrate.DesiredResult) error {
	if !isWholeRunRefusal(result) {
		if _, err := fmt.Fprintf(out, "%s: %s\n", result.Outcome, result.Detail); err != nil {
			return fmt.Errorf("write desired result: %w", err)
		}
		return nil
	}
	var b strings.Builder
	b.WriteString(pal.severity("error", fmt.Sprintf("refused (%s)", result.Reason)))
	fmt.Fprintf(&b, "\n  %s  %s", pal.bold("class:"), result.Class)
	if result.Owner != "" {
		fmt.Fprintf(&b, "\n  %s  %s", pal.bold("owner:"), result.Owner)
	}
	if result.Cause != verdict.CauseNone {
		fmt.Fprintf(&b, "\n  %s  %s", pal.bold("cause:"), result.Cause)
	}
	fmt.Fprintf(&b, "\n  %s %s", pal.bold("detail:"), result.Detail)
	if _, err := fmt.Fprintln(out, b.String()); err != nil {
		return fmt.Errorf("write desired result: %w", err)
	}
	return nil
}

// isWholeRunRefusal reports a classified refusal that no statement verdict
// carries: the run was refused before any statement was planned or run.
func isWholeRunRefusal(result migrate.DesiredResult) bool {
	return result.Outcome == verdict.OutcomeRefused && result.Reason != "" && len(result.Verdicts) == 0
}
