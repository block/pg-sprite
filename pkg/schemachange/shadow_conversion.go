package schemachange

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemadiff"
)

// ErrConversionNotImmutable is wrapped by the CauseStatementConversion
// refusal of a USING expression the server does not classify as
// IMMUTABLE: one that reads the clock, the session, or a setting, or
// calls a function declared STABLE or VOLATILE. The copy evaluates the
// expression once per row in the chunk copy, again in the verifier's
// recopy, and again for every image the catch-up flushes, at different
// times and in different sessions; only an immutable expression is
// guaranteed to give those evaluations the same answer for the same
// source value.
var ErrConversionNotImmutable = errors.New("USING expression is not immutable")

// codeInvalidObjectDefinition is the SQLSTATE the server answers when a
// stored generated column's expression is not immutable.
const codeInvalidObjectDefinition = "42P17"

// probeColumn is the name of the throwaway generated column the
// immutability probe adds inside a save point. It is rolled back before
// the gated statement runs, so no shadow ever carries it.
const probeColumn = "_pgsprite_using_probe"

// probeConversions asks the server whether every USING expression of the
// gated statement is immutable, before the statement runs. A stored
// generated column accepts only an immutable expression, so each
// conversion is tried as the generation expression of a throwaway column
// on the empty shadow, whose columns still have the source's types; the
// server reports a non-immutable expression as codeInvalidObjectDefinition,
// which becomes a refusal wrapping ErrConversionNotImmutable. Every other
// answer is the one the gated statement itself would get — a name that
// does not resolve, a cast that does not exist — and surfaces as the
// server's error. Each attempt runs in its own save point and is rolled
// back whether it succeeded or not, so the shadow the statement sees is
// the one the LIKE clone made.
//
// A generated source column is skipped: the server refuses USING on it
// when the statement runs, and a generation expression that reads a
// generated column is refused under the same SQLSTATE for a reason that
// is not immutability.
func probeConversions(ctx context.Context, tx pgx.Tx, target preflight.CopySwapTarget, shadow string, source schemadiff.Model, conversions map[string]string) error {
	generated := generatedColumns(source)
	columns := make([]string, 0, len(conversions))
	for column := range conversions {
		if !generated[column] {
			columns = append(columns, column)
		}
	}
	// Map order is random; the first refusal reported is the same on
	// every run.
	sort.Strings(columns)
	for _, column := range columns {
		if err := probeConversion(ctx, tx, target, shadow, column, conversions[column]); err != nil {
			return err
		}
	}
	return nil
}

// probeConversion tries one USING expression as a stored generated
// column's expression inside a save point that is always rolled back.
func probeConversion(ctx context.Context, tx pgx.Tx, target preflight.CopySwapTarget, shadow, column, expr string) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("save point before probing the USING expression of %s: %w", column, err)
	}
	table := pgx.Identifier{target.Schema(), shadow}.Sanitize()
	probe := pgx.Identifier{probeColumn}.Sanitize()
	// IS NOT NULL makes the probe's type boolean whatever the expression
	// yields, so the probe reads only the expression's volatility, never
	// whether its result type can be stored.
	_, execErr := sp.Exec(ctx, "ALTER TABLE "+table+" ADD COLUMN "+probe+" boolean GENERATED ALWAYS AS (("+expr+") IS NOT NULL) STORED")
	if rollbackErr := sp.Rollback(ctx); rollbackErr != nil {
		return errors.Join(execErr, fmt.Errorf("roll back the USING probe of %s: %w", column, rollbackErr))
	}
	if execErr == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(execErr, &pgErr) && pgErr.Code == codeInvalidObjectDefinition {
		// INV: ST-7
		return refuse(CauseStatementConversion, []error{ErrConversionNotImmutable, execErr}, "the copy cannot reproduce the USING expression of %s in the statement for %s.%s: %s", column, target.Schema(), target.Table(), pgErr.Message)
	}
	return fmt.Errorf("probe the USING expression of %s in the statement for %s.%s: %w", column, target.Schema(), target.Table(), execErr)
}

// generatedColumns names the generated columns of a model.
func generatedColumns(m schemadiff.Model) map[string]bool {
	generated := make(map[string]bool)
	for _, c := range m.Columns {
		if c.Generated {
			generated[c.Name] = true
		}
	}
	return generated
}
