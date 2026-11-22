package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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
		if c.AllowDestructive != "" {
			return errors.New("--allow-destructive cannot be combined with --desired: desired-state execution refuses a destructive plan whole")
		}
		if c.BlockWrites != "" {
			return errors.New("--block-writes cannot be combined with --desired: the plan, not the operator, decides how many copies would block the table's writes; run each statement with migrate --alter --block-writes instead")
		}
	}
	return nil
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
		// discard live structure, which RunDesired refuses before execution.
		plan.RefuseDestructive(&report)
		diff := DiffCmd{DBFlags: c.DBFlags, OutputFlags: c.OutputFlags, Schema: c.Schema, ExtensionSchema: c.ExtensionSchema, JSON: c.JSON}
		return diff.writeDiffReport(out, report)
	}
	opts, stopProgress, err := c.startProgress(ctx, c.options(c.diag()))
	if err != nil {
		return err
	}
	result, runErr := migrate.RunDesired(ctx, pool, migrate.DesiredRequest{Schema: c.Schema, Desired: desired}, opts)
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

// writeDesiredSummary writes the run's closing line. A classified refusal
// gets the verdict layout's headline, class, owner, and cause, because a
// whole-run refusal (a file refused before anything planned) carries no
// statement verdict that would show them; every other outcome is one line.
func writeDesiredSummary(out io.Writer, pal palette, result migrate.DesiredResult) error {
	if result.Outcome != verdict.OutcomeRefused || result.Reason == "" {
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
