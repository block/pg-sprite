package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// diffReport is the diff command's JSON output contract.
type diffReport struct {
	// Schema is the live schema the diff targeted.
	Schema string `json:"schema"`
	// Table is the desired (and live) table name.
	Table string `json:"table"`
	// TableExists reports whether the live table was found; when false the
	// changes are the full desired schema.
	TableExists bool `json:"table_exists"`
	// Changes is the ordered statement plan; empty means the live table
	// already matches the desired state.
	Changes []schemadiff.Change `json:"changes"`
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

	report := diffReport{Schema: c.Schema, Table: ds.Table, TableExists: true}
	live, err := schemadiff.Introspect(ctx, pool, c.Schema, ds.Table)
	switch {
	case errors.Is(err, schemadiff.ErrTableNotFound):
		// No live table: the plan is the desired schema itself, qualified
		// onto the target schema.
		report.TableExists = false
		if report.Changes, err = qualifiedDesired(ds, c.Schema); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		desired, err := schemadiff.IntrospectDesired(ctx, pool, ds)
		if err != nil {
			return err
		}
		if report.Changes, err = schemadiff.Diff(c.Schema, live, desired); err != nil {
			return err
		}
	}
	logger.Debug("diff derived",
		"schema", c.Schema, "table", ds.Table, "changes", len(report.Changes), "table_exists", report.TableExists)

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
		changes = append(changes, schemadiff.Change{SQL: qualified})
	}
	return changes, nil
}

// writeJSON emits the report as JSON.
func writeJSON(out io.Writer, report diffReport) error {
	if report.Changes == nil {
		report.Changes = []schemadiff.Change{}
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		return fmt.Errorf("write diff report: %w", err)
	}
	return nil
}

// writePlanText emits the plan as an executable SQL script: one statement
// per line, destructive statements flagged with a leading comment line, and
// SQL comments for the no-change and missing-table cases so the output stays
// valid SQL.
func writePlanText(out io.Writer, report diffReport) error {
	if len(report.Changes) == 0 {
		if _, err := fmt.Fprintln(out, "-- no changes: live table matches the desired schema"); err != nil {
			return fmt.Errorf("write plan: %w", err)
		}
		return nil
	}
	if !report.TableExists {
		if _, err := fmt.Fprintf(out, "-- table %s.%s does not exist; the plan is the full desired schema\n",
			report.Schema, report.Table); err != nil {
			return fmt.Errorf("write plan: %w", err)
		}
	}
	for _, ch := range report.Changes {
		if ch.Destructive {
			if _, err := fmt.Fprintln(out, "-- destructive"); err != nil {
				return fmt.Errorf("write plan: %w", err)
			}
		}
		if _, err := fmt.Fprintf(out, "%s;\n", ch.SQL); err != nil {
			return fmt.Errorf("write plan: %w", err)
		}
	}
	return nil
}

// runFmt canonicalizes a desired-state schema file: every statement is
// parsed through the PostgreSQL grammar, admitted by the same rules as diff,
// and printed back in the deparser's canonical form. Offline — no database.
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
