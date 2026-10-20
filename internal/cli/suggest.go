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
	return writeSuggestText(out, c.palette(out), sourceName(c.Path), report)
}

// writeSuggestText renders the suggestions in the same compiler-diagnostic
// grammar as the dry-run and diff reports: the risky statement leads each
// group under the conventional name:line:column: label, the classification
// is a warning entry beneath it, and the safer sequence (or the guidance
// naming the manual path) follows as a help entry. Display only — the JSON
// report is the machine contract; the codes are the same typed values the
// JSON report carries. A report with no suggestions prints nothing.
func writeSuggestText(out io.Writer, pal palette, name string, report suggest.Report) error {
	if len(report.Suggestions) == 0 {
		return nil
	}
	w := &stickyWriter{out: out, pal: pal}
	statement := 0
	var docs []string
	for _, s := range report.Suggestions {
		if s.Statement != statement {
			writeDocs(w, docs)
			docs = nil
			w.entry(fmt.Sprintf("%s:%d:%d:", name, s.Line, s.Column))
			w.printf("  %s;\n", s.Original)
			statement = s.Statement
		}
		w.diag("warning", string(s.Reason),
			fmt.Sprintf("%s — %s", s.Operation, impactText(s.Reason)))
		docs = appendCode(docs, onlineDDLReferenceURL+"#"+string(s.Reason))
		if len(s.Recommended) == 0 {
			w.diag("help", string(s.Guidance), suggest.ManualPath(s.Guidance))
			docs = appendCode(docs, suggestReportURL+"#"+string(s.Guidance))
			continue
		}
		w.diag("help", "", "a safer online form exists — not a semantic equivalent, and running it by hand forgoes the engine's execution-time guards:")
		for n, sql := range s.Recommended {
			w.printf("  %d. %s;\n", n+1, sql)
		}
		w.diag("note", "", "caveats: "+joinCaveats(s.Caveats))
		docs = appendCode(docs, suggestReportURL+"#caveats-caveats")
	}
	writeDocs(w, docs)
	w.entry("suggest:")
	w.printf("  %s — %s; advisory only, nothing was executed\n", name,
		countNoun(len(report.Suggestions), "suggestion"))
	return w.err
}

// joinCaveats renders the typed caveats as a comma-separated list.
func joinCaveats(caveats []suggest.Caveat) string {
	names := make([]string, len(caveats))
	for i, c := range caveats {
		names[i] = string(c)
	}
	return strings.Join(names, ", ")
}
