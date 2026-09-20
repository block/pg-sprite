package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

func (c *DiffCmd) runRowSecurityDiff(ctx context.Context, out io.Writer, sql string) error {
	desired, err := statement.ParseDesiredWithRowSecurity(sql)
	if err != nil {
		return err
	}
	pool, err := dbconn.NewPool(ctx, c.Config())
	if err != nil {
		return err
	}
	defer pool.Close()
	report, err := diffplan.PlanWithRowSecurity(ctx, pool, c.Schema, desired)
	if err != nil {
		if errors.Is(err, schemadiff.ErrUnsupportedChange) {
			return c.writeRowSecurityRefusal(out, err)
		}
		return err
	}
	return c.writeDiffReport(out, report)
}

// A refused comparison has no executable plan. Emit the existing verdict shape
// instead of an empty plan that could be mistaken for successful convergence.
func (c *DiffCmd) writeRowSecurityRefusal(out io.Writer, cause error) error {
	v := verdict.Verdict{Outcome: verdict.OutcomeRefused, Reason: verdict.ReasonUnsupportedStatement, Detail: cause.Error()}
	if c.JSON {
		text, err := v.JSON()
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(out, text); err != nil {
			return fmt.Errorf("write RLS refusal: %w", err)
		}
	} else if c.SQL {
		if _, err := fmt.Fprintln(out, "-- refused: "+strings.ReplaceAll(cause.Error(), "\n", "\n-- ")); err != nil {
			return fmt.Errorf("write RLS refusal: %w", err)
		}
	} else if err := writeVerdictText(out, c.palette(out), v); err != nil {
		return err
	}
	return errors.Join(verdict.ErrRefused, cause)
}
