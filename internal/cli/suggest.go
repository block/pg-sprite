package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/block/pg-sprite/pkg/suggest"
)

// runSuggest maps a DDL script to its advisory rewrites: parse every
// statement through the PostgreSQL grammar, classify it with zero live
// facts, and report the safer native form for anything risky as written.
// Offline — no database, nothing executes. A script with no rewrites
// prints nothing and the command always exits zero: suggest advises, lint
// gates.
func (c *SuggestCmd) runSuggest(in io.Reader, out io.Writer) error {
	var src []byte
	var err error
	if c.Path == "" {
		if src, err = io.ReadAll(in); err != nil {
			return fmt.Errorf("read DDL from stdin: %w", err)
		}
	} else if src, err = os.ReadFile(c.Path); err != nil {
		return fmt.Errorf("read DDL file: %w", err)
	}
	report, err := suggest.Advise(string(src))
	if err != nil {
		return err
	}
	if c.JSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return fmt.Errorf("write suggest report: %w", err)
		}
		return nil
	}
	return writeSuggestText(out, report)
}

// writeSuggestText renders each suggestion as the original, the safer
// sequence, and its caveats. A report with no suggestions prints nothing.
func writeSuggestText(out io.Writer, report suggest.Report) error {
	for _, s := range report.Suggestions {
		if _, err := fmt.Fprintf(out, "statement %d: %s — %s\n", s.Statement, s.Operation, s.Reason); err != nil {
			return fmt.Errorf("write suggest report: %w", err)
		}
		if _, err := fmt.Fprintf(out, "  safer form (not equivalent — see docs/postgres-online-ddl-reference.md):\n"); err != nil {
			return fmt.Errorf("write suggest report: %w", err)
		}
		for _, sql := range s.Recommended {
			if _, err := fmt.Fprintf(out, "    %s;\n", sql); err != nil {
				return fmt.Errorf("write suggest report: %w", err)
			}
		}
		if _, err := fmt.Fprintf(out, "  caveats: %s\n", joinCaveats(s.Caveats)); err != nil {
			return fmt.Errorf("write suggest report: %w", err)
		}
	}
	return nil
}

// joinCaveats renders the typed caveats as a comma-separated list.
func joinCaveats(caveats []suggest.Caveat) string {
	names := make([]string, len(caveats))
	for i, c := range caveats {
		names[i] = string(c)
	}
	return strings.Join(names, ", ")
}
