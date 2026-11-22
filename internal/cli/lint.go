package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/block/pg-sprite/pkg/lint"
	"github.com/block/pg-sprite/pkg/statement"
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
	if c.Desired {
		return c.lintDesired(out, string(src))
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

// lintDesired checks a desired schema file offline with the file admission
// diff and migrate --desired share before they connect: a file that is not one
// table with its indexes and row security, a types or partitions file, or a
// statement such a file may not hold is refused with its verdict and exits
// 2. An admitted file is clean: text prints nothing and JSON prints the
// empty lint report. Its statements are declarations, not operations, so the
// script rules do not apply to them; how the file differs from a live table
// is diff's to report, because only diff reads the table, and so is a table
// shape refused when the server builds it: admission does not mean planning
// will succeed.
func (c *LintCmd) lintDesired(out io.Writer, src string) error {
	if _, err := statement.ParseDesiredFile(src); err != nil {
		return desiredFileRefusal(out, c.palette(out), c.JSON, false, err, "")
	}
	if !c.JSON {
		return nil
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(lint.EmptyReport()); err != nil {
		return fmt.Errorf("write lint report: %w", err)
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
