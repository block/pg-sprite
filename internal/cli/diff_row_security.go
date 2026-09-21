package cli

import (
	"context"
	"encoding/json"
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
	var review *diffplan.RowSecurityReviewRequired
	errors.As(cause, &review)
	v := verdict.Verdict{Outcome: verdict.OutcomeRefused, Reason: verdict.ReasonUnsupportedStatement, Detail: cause.Error()}
	switch {
	case c.JSON && review != nil:
		report := struct {
			verdict.Verdict
			Schema string                       `json:"schema"`
			Table  string                       `json:"table"`
			Review schemadiff.RowSecurityReview `json:"row_security_review"`
		}{v, review.Schema, review.Table, review.Review}
		if err := json.NewEncoder(out).Encode(report); err != nil {
			return fmt.Errorf("write RLS review: %w", err)
		}
	case c.JSON:
		text, err := v.JSON()
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(out, text); err != nil {
			return fmt.Errorf("write RLS refusal: %w", err)
		}
	case c.SQL:
		if review != nil {
			var text strings.Builder
			if err := writeRowSecurityReview(&text, review); err != nil {
				return err
			}
			if _, err := fmt.Fprintln(out, sqlDiagnosticComment(strings.TrimSuffix(text.String(), "\n"))); err != nil {
				return fmt.Errorf("write RLS review: %w", err)
			}
		}
		if _, err := fmt.Fprintln(out, sqlDiagnosticComment("refused: "+cause.Error())); err != nil {
			return fmt.Errorf("write RLS refusal: %w", err)
		}
	default:
		if review != nil {
			if err := writeRowSecurityReview(out, review); err != nil {
				return err
			}
		}
		if err := writeVerdictText(out, c.palette(out), v); err != nil {
			return err
		}
	}
	return errors.Join(verdict.ErrRefused, cause)
}

// PostgreSQL ends a line comment at either CR or LF. Normalize both before
// prefixing every line, including untrusted identifiers and diagnostic text.
func sqlDiagnosticComment(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return "-- " + strings.ReplaceAll(text, "\n", "\n-- ")
}
