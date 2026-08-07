package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/block/pg-sprite/pkg/lint"
)

// ErrLintFindings is returned by lint when the script has error-severity
// findings, so the process exits non-zero; warnings alone pass.
var ErrLintFindings = errors.New("lint found errors")

// runLint lints a DDL script: parse every statement through the PostgreSQL
// grammar, classify it with zero live facts, and report typed findings.
// Offline — no database. A clean script prints nothing.
func (c *LintCmd) runLint(in io.Reader, out io.Writer) error {
	var src []byte
	var err error
	if c.Path == "" {
		if src, err = io.ReadAll(in); err != nil {
			return fmt.Errorf("read DDL from stdin: %w", err)
		}
	} else if src, err = os.ReadFile(c.Path); err != nil {
		return fmt.Errorf("read DDL file: %w", err)
	}
	report, err := lint.Check(string(src))
	if err != nil {
		return err
	}
	if c.JSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return fmt.Errorf("write lint report: %w", err)
		}
	} else if err := writeLintText(out, c.sourceName(), report); err != nil {
		return err
	}
	if report.Errors > 0 {
		return fmt.Errorf("%w: %d", ErrLintFindings, report.Errors)
	}
	return nil
}

// sourceName names the linted source for text findings: the file path, or
// the conventional "<stdin>" when the script came from the pipe.
func (c *LintCmd) sourceName() string {
	if c.Path == "" {
		return "<stdin>"
	}
	return c.Path
}

// writeLintText renders the findings in the conventional linter shape —
// name:line:column: severity — one per line with any suggestion indented
// beneath it, so CI systems and editors can jump to the source. A clean
// report prints nothing.
func writeLintText(out io.Writer, name string, report lint.Report) error {
	for _, f := range report.Findings {
		if _, err := fmt.Fprintf(out, "%s:%d:%d: %s: %s — %s\n",
			name, f.Line, f.Column, f.Severity, f.Code, f.Operation); err != nil {
			return fmt.Errorf("write lint report: %w", err)
		}
		if len(f.Suggestion) > 0 {
			if _, err := fmt.Fprintf(out, "  run instead: %s;\n",
				strings.Join(f.Suggestion, ";\n  ")); err != nil {
				return fmt.Errorf("write lint report: %w", err)
			}
		}
	}
	return nil
}
