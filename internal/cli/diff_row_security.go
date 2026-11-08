package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

func (c *DiffCmd) runRowSecurityDiff(ctx context.Context, out io.Writer, desired statement.DesiredWithRowSecurity, b executor.Budget) error {
	pool, err := dbconn.NewPool(ctx, c.Config())
	if err != nil {
		return err
	}
	defer pool.Close()
	report, err := diffplan.PlanWithRowSecurity(ctx, pool, c.Schema, desired)
	if errors.Is(err, schemadiff.ErrTableNotFound) {
		return c.previewCreateWithRowSecurity(ctx, out, pool, desired, b)
	}
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
	v := verdict.Verdict{Detail: cause.Error()}.WithRefusal(plan.RowSecurityReviewRefusal())
	if review == nil {
		return c.writeVerdictRefusal(out, v, cause)
	}
	switch {
	case c.JSON:
		report := struct {
			verdict.Verdict
			Schema string                       `json:"schema"`
			Table  string                       `json:"table"`
			Review schemadiff.RowSecurityReview `json:"row_security_review"`
		}{v, review.Schema, review.Table, review.Review}
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			return fmt.Errorf("write RLS review: %w", err)
		}
	case c.SQL:
		var text strings.Builder
		if err := writeRowSecurityReview(&text, review); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(out, sqlDiagnosticComment(strings.TrimSuffix(text.String(), "\n"))); err != nil {
			return fmt.Errorf("write RLS review: %w", err)
		}
		if _, err := fmt.Fprintln(out, sqlDiagnosticComment("refused: "+cause.Error())); err != nil {
			return fmt.Errorf("write RLS refusal: %w", err)
		}
	default:
		if err := writeRowSecurityReview(out, review); err != nil {
			return err
		}
		if err := writeVerdictText(out, c.palette(out), v); err != nil {
			return err
		}
	}
	return errors.Join(verdict.ErrRefused, cause)
}

// previewCreateWithRowSecurity shows what migrate --desired runs for a table
// that does not exist yet: the CREATE TABLE, its indexes, and its row
// security, all in one transaction. The preview runs that transaction,
// including its readback, and rolls it back.
func (c *DiffCmd) previewCreateWithRowSecurity(ctx context.Context, out io.Writer, pool *pgxpool.Pool, desired statement.DesiredWithRowSecurity, b executor.Budget) error {
	opts := migrate.Options{Budget: executor.SequenceBudget{Brief: b}, Logger: c.diag()}
	preview, err := migrate.PreviewCreateWithRowSecurity(ctx, pool, c.Schema, desired, opts)
	if err != nil {
		refusal, ok := migrate.RowSecurityRefusal(err)
		if !ok {
			return err
		}
		return c.writeVerdictRefusal(out, verdict.Verdict{Table: c.Schema + "." + desired.Table(), Detail: err.Error()}.WithRefusal(refusal), err)
	}
	switch {
	case c.JSON:
		report := struct {
			executor.RowSecurityPlan
			TableExists bool `json:"table_exists"`
		}{preview, false}
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			return fmt.Errorf("write create with row security preview: %w", err)
		}
	case c.SQL:
		header := sqlDiagnosticComment(fmt.Sprintf("%s.%s does not exist; pg-sprite migrate --desired creates it with its row security in one transaction",
			preview.Schema, preview.Table))
		if _, err := fmt.Fprintln(out, header); err != nil {
			return fmt.Errorf("write create with row security preview: %w", err)
		}
		for _, sql := range preview.Statements {
			if _, err := fmt.Fprintln(out, sql+";"); err != nil {
				return fmt.Errorf("write create with row security preview: %w", err)
			}
		}
	default:
		if _, err := fmt.Fprintf(out, "%s.%s does not exist. migrate --desired creates it with its row security in one transaction:\n",
			preview.Schema, preview.Table); err != nil {
			return fmt.Errorf("write create with row security preview: %w", err)
		}
		for _, sql := range preview.Statements {
			if _, err := fmt.Fprintf(out, "  %s;\n", sql); err != nil {
				return fmt.Errorf("write create with row security preview: %w", err)
			}
		}
	}
	return nil
}

// writeVerdictRefusal emits a refusal verdict in the requested format and
// returns the refused exit error.
func (c *DiffCmd) writeVerdictRefusal(out io.Writer, v verdict.Verdict, cause error) error {
	switch {
	case c.JSON:
		text, err := v.JSON()
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(out, text); err != nil {
			return fmt.Errorf("write refusal: %w", err)
		}
	case c.SQL:
		if _, err := fmt.Fprintln(out, sqlDiagnosticComment("refused: "+cause.Error())); err != nil {
			return fmt.Errorf("write refusal: %w", err)
		}
	default:
		if err := writeVerdictText(out, c.palette(out), v); err != nil {
			return err
		}
	}
	return errors.Join(verdict.ErrRefused, cause)
}
