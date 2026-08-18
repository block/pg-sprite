package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/block/pg-sprite/pkg/lint"
)

// ErrLintFindings is returned by lint when the script has error-severity
// findings, so the process exits non-zero; warnings alone pass.
var ErrLintFindings = errors.New("lint found errors")

// onlineDDLReferenceURL is where output that recommends a safer form sends
// the reader. A URL rather than a repo path: an installed build has no
// docs/ tree, so the reference must be reachable from the string alone.
const onlineDDLReferenceURL = "https://github.com/block/pg-sprite/blob/main/docs/postgres-online-ddl-reference.md"

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
	} else if err := writeLintText(out, c.palette(out), sourceName(c.Path), report); err != nil {
		return err
	}
	if report.Errors > 0 {
		return fmt.Errorf("%w: %d", ErrLintFindings, report.Errors)
	}
	return nil
}

// sourceName names a command's DDL source for text findings: the file
// path, or the conventional "<stdin>" when the script came from the pipe.
func sourceName(path string) string {
	if path == "" {
		return "<stdin>"
	}
	return path
}
