// Package diffplan is the declarative front door as a library: a parsed
// desired-state schema in, the routed convergence plan out. The CLI's diff
// command and orchestrators embedding pg-sprite share this one pipeline, so
// a stored report means the same thing no matter which caller produced it.
//
// Callers own the boundary concerns: parse the desired file through
// [statement.ParseDesired] (refusals surface at the caller) and build the
// connection through [dbconn.NewPool]. Plan never writes the live table,
// but it is not read-only either — desired state is realized by
// execute-and-introspect on a rolled-back scratch schema, so the
// connection requirements on [Plan] apply.
//
// Before a v1 module tag the Go API carries no compatibility promise: the
// JSON [plan.Report] is the stability boundary, the Go API follows at v1
// (see docs/architecture.md).
package diffplan

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// Request names the inputs to [Plan]. Zero-value fields are invalid: the
// schema must be set, and the desired state must come from
// [statement.ParseDesired] — the zero DesiredSchema is refused.
type Request struct {
	// Schema is the target schema the desired table lives in.
	Schema string
	// Desired is the parsed desired-state schema for the table.
	Desired statement.DesiredSchema
}

// Plan derives the ordered, classified, and routed convergence plan for the
// desired schema against the live database: introspect the live table,
// derive the changes (the full qualified desired schema when the table does
// not exist yet), classify each change with live-column facts, route the
// set, and stamp the report with the server version and fingerprint.
//
// Plan needs more than a read-only connection: desired state is realized by
// executing the desired DDL in a scratch schema inside a transaction that
// is always rolled back, so the pool must connect read-write (not a hot
// standby) as a role with CREATE privilege on the target database. The live
// table is introspected only — never written. Plan does not close the pool;
// one pool serves any number of calls.
func Plan(ctx context.Context, pool *pgxpool.Pool, req Request) (plan.Report, error) {
	if req.Schema == "" {
		return plan.Report{}, errors.New("plan desired schema: schema name is required")
	}
	ds := req.Desired
	if ds.Table() == "" {
		return plan.Report{}, errors.New("plan desired schema: desired state names no table")
	}

	report := plan.NewReport(plan.SourceDiff)
	report.Schema = req.Schema
	report.Table = ds.Table()
	var err error
	if report.ServerVersion, err = dbconn.ServerVersion(ctx, pool); err != nil {
		return plan.Report{}, err
	}

	tableExists := true
	var changes []schemadiff.Change
	var facts planner.Facts
	live, err := schemadiff.Introspect(ctx, pool, req.Schema, ds.Table())
	switch {
	case errors.Is(err, schemadiff.ErrTableNotFound):
		// No live table: the plan is the desired schema itself, qualified
		// onto the target schema, classified with zero facts (there are no
		// live columns to sharpen type-change decisions).
		tableExists = false
		if changes, err = qualifiedDesired(ds, req.Schema); err != nil {
			return plan.Report{}, err
		}
	case err != nil:
		return plan.Report{}, err
	default:
		facts = planner.FactsFrom(live)
		desired, err := schemadiff.IntrospectDesired(ctx, pool, ds)
		if err != nil {
			return plan.Report{}, err
		}
		if changes, err = schemadiff.Diff(req.Schema, live, desired); err != nil {
			return plan.Report{}, err
		}
	}
	report.TableExists = &tableExists
	if report.Statements, report.Disposition, err = classifyChanges(changes, facts); err != nil {
		return plan.Report{}, err
	}
	if !tableExists {
		plan.DiscloseGreenfieldExecution(&report)
	} else {
		targetFacts, checkErr := preflight.LookupTargetFacts(ctx, pool, req.Schema, ds.Table())
		if checkErr != nil {
			return plan.Report{}, checkErr
		}
		if targetFacts.Partitioned() {
			refused := make([]bool, len(report.Statements))
			for i := range report.Statements {
				cause, causeErr := preflight.RefusesPartitionedParent(targetFacts.ServerMajor(), report.Statements[i].ExecSQL)
				if causeErr != nil {
					return plan.Report{}, causeErr
				}
				refused[i] = cause != ""
			}
			plan.RefuseUnsupportedPartitionedParent(&report, refused)
		}
	}
	report.Fingerprint = plan.Fingerprint(report.Statements)
	return report, nil
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
		ps, err := plan.FromRouted(routed.Statements[i])
		if err != nil {
			return nil, "", fmt.Errorf("plan derived statement %q: %w", routed.Statements[i].Statement, err)
		}
		ps.Kind = ch.Kind
		planned = append(planned, ps)
	}
	return planned, routed.Disposition, nil
}

// qualifiedDesired renders the desired statements as the plan for a table
// that does not exist yet, qualified onto the target schema. The statements
// arrive in execution order — the CREATE TABLE first, the indexes in input
// order after it, ordered once by statement.ParseDesired — so the plan
// states the exact order the create path executes and a plan statement's
// verdict is the verdict of the step at the same position.
func qualifiedDesired(ds statement.DesiredSchema, schema string) ([]schemadiff.Change, error) {
	statements := ds.Statements()
	if len(statements) == 0 || statements[0].Kind() != statement.KindCreateTable {
		// INV: ST-8 — a DesiredSchema proof guarantees a CREATE TABLE
		// ordered first; a set that does not lead with one means the proof
		// was forged or mutated.
		return nil, errors.New("desired schema does not lead with a CREATE TABLE")
	}
	changes := make([]schemadiff.Change, 0, len(statements))
	for _, st := range statements {
		qualified, err := statement.Qualify(st.SQL(), schema)
		if err != nil {
			return nil, fmt.Errorf("qualify desired statement: %w", err)
		}
		kind := schemadiff.ChangeCreateIndex
		if st.Kind() == statement.KindCreateTable {
			kind = schemadiff.ChangeCreateTable
		}
		changes = append(changes, schemadiff.Change{SQL: qualified, Kind: kind})
	}
	return changes, nil
}
