// Package diffplan is the declarative front door as a library: a parsed
// desired-state schema in, the routed convergence plan out. The CLI's diff
// command and orchestrators embedding pg-sprite share this one pipeline, so
// a stored report means the same thing no matter which caller produced it.
//
// Callers own the boundary concerns: parse the desired file through
// [statement.ParseDesired] (refusals surface at the caller) and build the
// connection through [dbconn.NewPool]. Plan never executes anything against
// the live table — desired state is realized by execute-and-introspect on a
// rolled-back scratch schema.
package diffplan

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// Plan derives the ordered, classified, and routed convergence plan for the
// desired schema against the live database: introspect the live table,
// derive the changes (the full qualified desired schema when the table does
// not exist yet), classify each change with live-column facts, route the
// set, and stamp the report with the server version and fingerprint.
// Nothing is ever executed against the live table.
func Plan(ctx context.Context, pool *pgxpool.Pool, schema string, ds statement.DesiredSchema) (plan.Report, error) {
	if schema == "" {
		return plan.Report{}, errors.New("plan desired schema: schema name is required")
	}
	if ds.Table == "" {
		return plan.Report{}, errors.New("plan desired schema: desired state names no table")
	}

	report := plan.NewReport(plan.SourceDiff)
	report.Schema = schema
	report.Table = ds.Table
	var err error
	if report.ServerVersion, err = dbconn.ServerVersion(ctx, pool); err != nil {
		return plan.Report{}, err
	}

	tableExists := true
	var changes []schemadiff.Change
	var facts planner.Facts
	live, err := schemadiff.Introspect(ctx, pool, schema, ds.Table)
	switch {
	case errors.Is(err, schemadiff.ErrTableNotFound):
		// No live table: the plan is the desired schema itself, qualified
		// onto the target schema, classified with zero facts (there are no
		// live columns to sharpen type-change decisions).
		tableExists = false
		if changes, err = qualifiedDesired(ds, schema); err != nil {
			return plan.Report{}, err
		}
	case err != nil:
		return plan.Report{}, err
	default:
		facts = LiveFacts(live)
		desired, err := schemadiff.IntrospectDesired(ctx, pool, ds)
		if err != nil {
			return plan.Report{}, err
		}
		if changes, err = schemadiff.Diff(schema, live, desired); err != nil {
			return plan.Report{}, err
		}
	}
	report.TableExists = &tableExists
	if report.Statements, report.Disposition, err = classifyChanges(changes, facts); err != nil {
		return plan.Report{}, err
	}
	report.Fingerprint = plan.Fingerprint(report.Statements)
	return report, nil
}

// LiveFacts extracts the planner facts the live model provides: the
// canonical type of every live column. Both front doors — the declarative
// diff and the imperative dry-run — extract facts through this one function
// so equivalent changes classify identically.
func LiveFacts(live schemadiff.Model) planner.Facts {
	types := make(map[string]string, len(live.Columns))
	for _, col := range live.Columns {
		types[col.Name] = col.Type
	}
	return planner.Facts{ColumnTypes: types}
}

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
