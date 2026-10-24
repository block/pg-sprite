package migrate

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
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

	// Schema is the schema the facts describe: the statement's
	// qualification, or the schema an unqualified table resolves to on the
	// session search_path, the one Run resolves it to. An unqualified table
	// the search_path does not show is looked up in public, and reported
	// missing; a statement without a single table target carries its own
	// qualification.
	Schema string

	// Table is the table the facts describe: the statement's target, or,
	// for an ALTER INDEX ... RENAME TO, which names no table, the table
	// that owns the renamed index; empty for a statement without a single
	// table target.
	Table string

	// TableExists mirrors the introspection outcome for a plan report:
	// true when the table was found, false when it was looked up and
	// missing, nil when the statement has no single table target to
	// introspect.
	TableExists *bool
}

// LiveFacts introspects the statement's target table for classifier facts.
// For an ALTER TABLE that retypes or drops a column it reads which views
// depend on each column (preflight.ColumnDependentViews), and for one that
// adds columns, has the
// server prove whether each one rewrites the table
// (preflight.AddColumnRewrites). For one that adds an unnamed CHECK or
// FOREIGN KEY it also reads the constraint names the schema already uses,
// so the safer sequence names the constraint as the server would.
func LiveFacts(ctx context.Context, pool *pgxpool.Pool,
	st statement.Statement) (Facts, error) {
	if st.Kind() == statement.KindRenameIndex {
		return renamedIndexFacts(ctx, pool, st)
	}
	if st.Table() == "" {
		return Facts{Schema: st.Schema()}, nil
	}
	schema, err := factsSchema(ctx, pool, st)
	if err != nil {
		return Facts{}, err
	}
	live, err := schemadiff.Introspect(ctx, pool, schema, st.Table())
	switch {
	case errors.Is(err, schemadiff.ErrTableNotFound):
		exists := false
		return Facts{Schema: schema, Table: st.Table(), TableExists: &exists}, nil
	case err != nil:
		return Facts{}, err
	}
	targetFacts, err := preflight.LookupTargetFacts(ctx, pool, schema, st.Table())
	if err != nil {
		return Facts{}, err
	}
	classifier := planner.FactsFrom(live)
	adds, err := addsColumn(st)
	if err != nil {
		return Facts{}, err
	}
	changes, err := retypesOrDropsColumn(st)
	if err != nil {
		return Facts{}, err
	}
	if changes {
		views, err := preflight.ColumnDependentViews(ctx, pool, schema, st.Table())
		if err != nil {
			return Facts{}, err
		}
		classifier.DependentViews, classifier.DropDependentViews, classifier.OwnDependentViews = views.Tree, views.Drop, views.Own
	}
	if adds {
		if classifier.AddColumnProofs, err = preflight.AddColumnRewrites(ctx, pool, schema, st.Table(), st.SQL()); err != nil {
			return Facts{}, err
		}
	}
	names, err := addsUnnamedValidatableConstraint(st)
	if err != nil {
		return Facts{}, err
	}
	if names {
		if classifier.ConstraintNames, err = preflight.SchemaConstraintNames(ctx, pool, schema); err != nil {
			return Facts{}, err
		}
	}
	exists := true
	return Facts{
		Schema:      schema,
		Table:       st.Table(),
		Classifier:  classifier,
		Target:      targetFacts,
		TableExists: &exists,
	}, nil
}

// factsSchema is the schema LiveFacts reads st's table in: its
// qualification, or where the session search_path resolves the unqualified
// name, so a dry-run reads the table Run would act on. A name the
// search_path does not show falls back to ResolvedSchema, where the table
// is then reported missing.
func factsSchema(ctx context.Context, pool *pgxpool.Pool, st statement.Statement) (string, error) {
	if st.Schema() != "" {
		return st.Schema(), nil
	}
	// INV: CO-9 — every function, operator, and relation is
	// pg_catalog-qualified, so a planted overload on the session's
	// search_path cannot resolve the name to another relation's schema.
	// to_regclass itself resolves the bare name on that path, as Run's
	// resolveTarget does.
	const q = `
		SELECT n.nspname
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		WHERE c.oid OPERATOR(pg_catalog.=) pg_catalog.to_regclass(pg_catalog.quote_ident($1))`
	var schema string
	err := pool.QueryRow(ctx, q, st.Table()).Scan(&schema)
	if errors.Is(err, pgx.ErrNoRows) {
		return ResolvedSchema(st), nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve %s against search_path: %w", st.Table(), err)
	}
	return schema, nil
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

// addsColumn reports whether st is an ALTER TABLE with an ADD COLUMN, the
// only statement whose classification the server's rewrite proof decides.
func addsColumn(st statement.Statement) (bool, error) {
	if st.Kind() != statement.KindAlterTable {
		return false, nil
	}
	ops, err := statement.ParseOps(st.SQL())
	if err != nil {
		return false, fmt.Errorf("read the operations of %q: %w", st.SQL(), err)
	}
	return slices.ContainsFunc(ops, func(op statement.Op) bool { return op.Kind == statement.OpAddColumn }), nil
}

// retypesOrDropsColumn reports whether st is an ALTER TABLE that changes a
// column's type or drops a column, the two changes a view reading the
// column blocks.
func retypesOrDropsColumn(st statement.Statement) (bool, error) {
	if st.Kind() != statement.KindAlterTable {
		return false, nil
	}
	ops, err := statement.ParseOps(st.SQL())
	if err != nil {
		return false, fmt.Errorf("read the operations of %q: %w", st.SQL(), err)
	}
	return slices.ContainsFunc(ops, func(op statement.Op) bool {
		return op.Kind == statement.OpAlterColumnType || op.Kind == statement.OpDropColumn
	}), nil
}

// addsUnnamedValidatableConstraint reports whether st is an ALTER TABLE
// adding a CHECK or FOREIGN KEY it does not name.
func addsUnnamedValidatableConstraint(st statement.Statement) (bool, error) {
	if st.Kind() != statement.KindAlterTable {
		return false, nil
	}
	ops, err := statement.ParseOps(st.SQL())
	if err != nil {
		return false, fmt.Errorf("read the operations of %q: %w", st.SQL(), err)
	}
	return slices.ContainsFunc(ops, func(op statement.Op) bool {
		validatable := op.Constraint == statement.ConstraintCheck || op.Constraint == statement.ConstraintForeignKey
		return op.Kind == statement.OpAddConstraint && validatable && op.Name == ""
	}), nil
}
