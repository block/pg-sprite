package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// classifyChanges routes every derived change through the shared
// classify-and-route pipeline. facts sharpen type-change classification;
// the zero value is valid and strictly more conservative.
func classifyChanges(changes []schemadiff.Change, facts planner.Facts) ([]plan.Statement, router.Disposition, error) {
	plans := make([]planner.Plan, 0, len(changes))
	for _, ch := range changes {
		// Canonicalize before classifying so the report carries the
		// engine's canonical rendering — the same string the alter front
		// door would report for the same change.
		canonical, err := statement.Canonical(ch.SQL)
		if err != nil {
			return nil, "", fmt.Errorf("canonicalize derived statement %q: %w", ch.SQL, err)
		}
		classified, err := planner.Classify(canonical, facts)
		if err != nil {
			return nil, "", fmt.Errorf("classify derived statement %q: %w", canonical, err)
		}
		plans = append(plans, classified)
	}
	routed := router.Route(plans)
	planned := make([]plan.Statement, 0, len(changes))
	for i, ch := range changes {
		ps := plan.FromRouted(routed.Statements[i])
		ps.Kind = ch.Kind
		planned = append(planned, ps)
	}
	return planned, routed.Disposition, nil
}

// liveFacts extracts the planner facts the live model provides: the
// canonical type of every live column.
func liveFacts(live schemadiff.Model) planner.Facts {
	types := make(map[string]string, len(live.Columns))
	for _, col := range live.Columns {
		types[col.Name] = col.Type
	}
	return planner.Facts{ColumnTypes: types}
}

// run is the diff flow: parse and admit the desired file, introspect the
// live table and the desired state (execute-and-introspect on a rolled-back
// scratch schema), and print the ordered plan. Nothing is ever executed
// against the live table.
func (c *DiffCmd) run(ctx context.Context, out io.Writer) error {
	logger := c.diag()
	raw, err := os.ReadFile(c.Desired)
	if err != nil {
		return fmt.Errorf("read desired schema: %w", err)
	}
	ds, err := statement.ParseDesired(string(raw))
	if err != nil {
		return err
	}
	logger.Debug("desired schema parsed", "table", ds.Table, "statements", len(ds.Statements))

	pool, err := dbconn.NewPool(ctx, c.Config())
	if err != nil {
		return err
	}
	defer pool.Close()

	report := plan.NewReport(plan.SourceDiff)
	report.Schema = c.Schema
	report.Table = ds.Table
	if report.ServerVersion, err = serverVersion(ctx, pool); err != nil {
		return err
	}
	tableExists := true
	var changes []schemadiff.Change
	var facts planner.Facts
	live, err := schemadiff.Introspect(ctx, pool, c.Schema, ds.Table)
	switch {
	case errors.Is(err, schemadiff.ErrTableNotFound):
		// No live table: the plan is the desired schema itself, qualified
		// onto the target schema, classified with zero facts (there are no
		// live columns to sharpen type-change decisions).
		tableExists = false
		if changes, err = qualifiedDesired(ds, c.Schema); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		facts = liveFacts(live)
		desired, err := schemadiff.IntrospectDesired(ctx, pool, ds)
		if err != nil {
			return err
		}
		if changes, err = schemadiff.Diff(c.Schema, live, desired); err != nil {
			return err
		}
	}
	report.TableExists = &tableExists
	if report.Statements, report.Disposition, err = classifyChanges(changes, facts); err != nil {
		return err
	}
	report.Fingerprint = plan.Fingerprint(report.Statements)
	logger.Debug("diff derived",
		"schema", c.Schema, "table", ds.Table, "changes", len(report.Statements),
		"table_exists", tableExists, "disposition", string(report.Disposition))

	if c.JSON {
		return writeJSON(out, report)
	}
	return writePlanText(out, report)
}

// qualifiedDesired renders the desired statements as the plan for a table
// that does not exist yet, qualified onto the target schema.
func qualifiedDesired(ds statement.DesiredSchema, schema string) ([]schemadiff.Change, error) {
	changes := make([]schemadiff.Change, 0, len(ds.Statements))
	for _, st := range ds.Statements {
		qualified, err := statement.Qualify(st.SQL(), schema)
		if err != nil {
			return nil, fmt.Errorf("qualify desired statement: %w", err)
		}
		kind := schemadiff.ChangeCreateTable
		if st.Kind() == statement.KindCreateIndex {
			kind = schemadiff.ChangeCreateIndex
		}
		changes = append(changes, schemadiff.Change{SQL: qualified, Kind: kind})
	}
	return changes, nil
}

// writeJSON emits the plan report as JSON.
func writeJSON(out io.Writer, report plan.Report) error {
	if report.Statements == nil {
		report.Statements = []plan.Statement{}
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		return fmt.Errorf("write plan report: %w", err)
	}
	return nil
}

// writePlanText emits the plan as an executable SQL script: one statement
// per line, each annotated with its route, destructive statements flagged,
// and SQL comments for the no-change and missing-table cases so the output
// stays valid SQL. Safer sequences appear as comment lines — never
// substituted into the script body, which stays the literal convergence
// plan (a CONCURRENTLY rewrite could not run inside a transaction block).
// The header points at migrate as the executing front door: running this
// script directly bypasses the gate that refuses blocking statements.
func writePlanText(out io.Writer, report plan.Report) error {
	if len(report.Statements) == 0 {
		if _, err := fmt.Fprintln(out, "-- no changes: live table matches the desired schema"); err != nil {
			return fmt.Errorf("write plan: %w", err)
		}
		return nil
	}
	if _, err := fmt.Fprintln(out, "-- plan derived by pg-sprite diff; execute statements via pg-sprite migrate,"); err != nil {
		return fmt.Errorf("write plan: %w", err)
	}
	if _, err := fmt.Fprintln(out, "-- which refuses blocking forms — running this script directly bypasses that gate"); err != nil {
		return fmt.Errorf("write plan: %w", err)
	}
	if report.TableExists != nil && !*report.TableExists {
		if _, err := fmt.Fprintf(out, "-- table %s.%s does not exist; the plan is the full desired schema\n",
			report.Schema, report.Table); err != nil {
			return fmt.Errorf("write plan: %w", err)
		}
	}
	for _, ps := range report.Statements {
		if err := writeChangeText(out, ps); err != nil {
			return err
		}
	}
	return nil
}

// writeChangeText emits one annotated statement of the text plan.
func writeChangeText(out io.Writer, ps plan.Statement) error {
	if _, err := fmt.Fprintf(out, "-- %s\n", annotate(ps)); err != nil {
		return fmt.Errorf("write plan: %w", err)
	}
	if len(ps.ExecSQL) > 0 && ps.ExecSQL[0] != ps.SQL {
		if _, err := fmt.Fprintln(out, "-- the engine would run instead:"); err != nil {
			return fmt.Errorf("write plan: %w", err)
		}
		for _, safer := range ps.ExecSQL {
			if _, err := fmt.Fprintf(out, "--   %s;\n", safer); err != nil {
				return fmt.Errorf("write plan: %w", err)
			}
		}
	}
	if ps.Destructive {
		if _, err := fmt.Fprintln(out, "-- destructive"); err != nil {
			return fmt.Errorf("write plan: %w", err)
		}
	}
	if _, err := fmt.Fprintf(out, "%s;\n", ps.SQL); err != nil {
		return fmt.Errorf("write plan: %w", err)
	}
	return nil
}

// annotate renders one statement's route annotation: the route, the
// distinct decision reasons, and the availability note for backends this
// build does not implement.
func annotate(ps plan.Statement) string {
	var reasons []string
	seen := map[planner.Reason]bool{}
	for _, d := range ps.Decisions {
		if !seen[d.Reason] {
			seen[d.Reason] = true
			reasons = append(reasons, string(d.Reason))
		}
	}
	s := fmt.Sprintf("%s (%s)", ps.Route, strings.Join(reasons, ", "))
	switch ps.Disposition {
	case router.DispositionUnavailable:
		s += ": needs the " + string(ps.Backend) + " backend, which is not implemented yet"
	case router.DispositionRewriteRequired:
		s += ": blocks as submitted and no online rewrite was constructed — the engine will not run it"
	}
	return s
}

// runFmt canonicalizes a desired-state schema file: every statement is
// parsed through the PostgreSQL grammar, admitted by the same rules as diff,
// and printed back in the deparser's canonical form. Offline — no database.
// Commented input is refused (statement.ErrCommentLoss): the parser drops
// comments, and a formatter must never silently discard content.
func (c *FmtCmd) runFmt(in io.Reader, out io.Writer) error {
	var src []byte
	var err error
	if c.Path == "" {
		if src, err = io.ReadAll(in); err != nil {
			return fmt.Errorf("read schema from stdin: %w", err)
		}
	} else if src, err = os.ReadFile(c.Path); err != nil {
		return fmt.Errorf("read schema file: %w", err)
	}
	if err := statement.CheckNoComments(string(src)); err != nil {
		return err
	}
	ds, err := statement.ParseDesired(string(src))
	if err != nil {
		return err
	}
	for _, st := range ds.Statements {
		if _, err := fmt.Fprintf(out, "%s;\n", st.SQL()); err != nil {
			return fmt.Errorf("write formatted schema: %w", err)
		}
	}
	return nil
}
