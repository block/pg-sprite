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

// Facts is one introspection pass over the statement's target table. Both
// [Run] and a dry-run plan classify from one Facts value, so execution and
// its plan describe the same live state.
type Facts struct {
	// Classifier feeds [planner.Classify]. Statements without a single
	// table target (index drops, REINDEX) and missing tables classify
	// with zero facts — a strictly more conservative plan.
	Classifier planner.Facts

	// Target carries the preflight facts (partitioning, server major)
	// for plan-time partition checks; zero whenever Classifier is.
	Target preflight.TargetFacts

	// TableExists mirrors the introspection outcome for a plan report:
	// true when the table was found, false when it was looked up and
	// missing, nil when the statement has no single table target to
	// introspect.
	TableExists *bool
}

// LiveFacts introspects the statement's target table for classifier facts.
func LiveFacts(ctx context.Context, pool *pgxpool.Pool,
	st statement.Statement) (Facts, error) {
	if st.Table() == "" {
		return Facts{}, nil
	}
	live, err := schemadiff.Introspect(ctx, pool, ResolvedSchema(st), st.Table())
	switch {
	case errors.Is(err, schemadiff.ErrTableNotFound):
		exists := false
		return Facts{TableExists: &exists}, nil
	case err != nil:
		return Facts{}, err
	}
	targetFacts, err := preflight.LookupTargetFacts(ctx, pool, ResolvedSchema(st), st.Table())
	if err != nil {
		return Facts{}, err
	}
	exists := true
	return Facts{
		Classifier:  planner.FactsFrom(live),
		Target:      targetFacts,
		TableExists: &exists,
	}, nil
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
