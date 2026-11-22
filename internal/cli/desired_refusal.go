package cli

import (
	"fmt"
	"io"

	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/verdict"
)

// desiredFileRefusal prints the refusal verdict for a desired file refused
// before anything was planned — a statement the file may not hold, a second
// table, or a table shape the planner cannot build — and returns
// verdict.ErrRefused, so the command exits with the refusal code. sql is the
// refused statement when the error does not name one. asJSON prints the
// verdict as JSON and asSQL as an SQL comment, so --sql output stays valid
// SQL. For any other error it prints nothing and returns err unchanged.
func desiredFileRefusal(out io.Writer, pal palette, asJSON, asSQL bool, err error, sql string) error {
	v, ok := migrate.DesiredFileRefusal(err, sql)
	if !ok {
		return err
	}
	switch {
	case asJSON:
		text, jsonErr := v.JSON()
		if jsonErr != nil {
			return jsonErr
		}
		if _, writeErr := fmt.Fprintln(out, text); writeErr != nil {
			return fmt.Errorf("write desired-file refusal: %w", writeErr)
		}
	case asSQL:
		if _, writeErr := fmt.Fprintln(out, sqlDiagnosticComment("refused: "+v.Detail)); writeErr != nil {
			return fmt.Errorf("write desired-file refusal: %w", writeErr)
		}
	default:
		if writeErr := writeVerdictText(out, pal, v); writeErr != nil {
			return writeErr
		}
	}
	return verdict.ErrRefused
}
