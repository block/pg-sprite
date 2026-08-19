package migrate

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// LiveFacts introspects the statement's target table for classifier facts.
// Statements without a single table target (index drops, REINDEX) and
// missing tables classify with zero facts. The returned tableExists mirrors
// the introspection outcome for a plan report: true when the table was
// found, false when it was looked up and missing, nil when the statement
// has no single table target to introspect. Both [Run] and a dry-run plan
// classify from this one lookup, so execution and its plan describe the
// same live state.
func LiveFacts(ctx context.Context, pool *pgxpool.Pool,
	st statement.Statement) (planner.Facts, preflight.TargetFacts, *bool, error) {
	if st.Table() == "" {
		return planner.Facts{}, preflight.TargetFacts{}, nil, nil
	}
	live, err := schemadiff.Introspect(ctx, pool, ResolvedSchema(st), st.Table())
	switch {
	case errors.Is(err, schemadiff.ErrTableNotFound):
		exists := false
		return planner.Facts{}, preflight.TargetFacts{}, &exists, nil
	case err != nil:
		return planner.Facts{}, preflight.TargetFacts{}, nil, err
	}
	targetFacts, err := preflight.LookupTargetFacts(ctx, pool, ResolvedSchema(st), st.Table())
	if err != nil {
		return planner.Facts{}, preflight.TargetFacts{}, nil, err
	}
	exists := true
	return planner.FactsFrom(live), targetFacts, &exists, nil
}

// ResolvedSchema is the schema the engine plans against: the statement's
// qualification, or public — the default the engine introspects — when a
// table-targeted statement leaves it unqualified. A report carries the
// resolved name, never the submitted one: a stored plan must not depend on
// the reader's search_path to say which table it describes.
func ResolvedSchema(st statement.Statement) string {
	if st.Schema() == "" && st.Table() != "" {
		return "public"
	}
	return st.Schema()
}
