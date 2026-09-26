package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

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
	}
	return nil
}

func (c *MigrateCmd) runDesired(ctx context.Context, out io.Writer) error {
	raw, err := os.ReadFile(c.Desired)
	if err != nil {
		return fmt.Errorf("read desired schema: %w", err)
	}
	desired, err := statement.ParseDesired(string(raw))
	if err != nil {
		carriesRLS, detectErr := statement.HasRowSecurityDeclaration(string(raw))
		if detectErr == nil && carriesRLS {
			return c.runDesiredRowSecurity(ctx, out, string(raw))
		}
		return err
	}
	pool, err := dbconn.NewPool(ctx, c.Config())
	if err != nil {
		return err
	}
	defer pool.Close()
	if c.DryRun {
		report, err := diffplan.Plan(ctx, pool, diffplan.Request{Schema: c.Schema, Desired: desired})
		if err != nil {
			return err
		}
		// Preview uses desired-state admission: a routed native step can still
		// discard live structure, which RunDesired refuses before execution.
		plan.RefuseDestructive(&report)
		diff := DiffCmd{DBFlags: c.DBFlags, OutputFlags: c.OutputFlags, JSON: c.JSON}
		return diff.writeDiffReport(out, report)
	}
	result, runErr := migrate.RunDesired(ctx, pool, migrate.DesiredRequest{Schema: c.Schema, Desired: desired}, c.options(c.diag()))
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
	for _, v := range result.Verdicts {
		if err := writeVerdictText(out, c.palette(out), v); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(out, "%s: %s\n", result.Outcome, result.Detail)
	return err
}
