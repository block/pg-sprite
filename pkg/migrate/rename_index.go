package migrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

var (
	// ErrRenamedIndexNotFound means the relation an ALTER INDEX ... RENAME
	// TO names is not an index visible on the session search_path, so
	// there is no table to plan or preflight. Nothing has executed.
	ErrRenamedIndexNotFound = errors.New("the renamed index is not an index visible on the session search_path")
	// ErrIndexRenameAcknowledgement means an acknowledgement was given for
	// an ALTER INDEX ... RENAME TO, which neither the force nor the
	// copy-and-swap restart acknowledgement applies to. Nothing has
	// executed.
	ErrIndexRenameAcknowledgement = errors.New("the acknowledgement does not apply to ALTER INDEX ... RENAME TO")
)

// renamedIndexFacts is LiveFacts for an ALTER INDEX ... RENAME TO: the
// statement names an index, so the facts describe the table that owns it,
// read from the catalog, and carry the constraint the index backs, which
// the rename would rename with it. An unqualified index name resolves on
// the session search_path, exactly as the statement will when it runs on
// the same pool.
func renamedIndexFacts(ctx context.Context, pool *pgxpool.Pool, st statement.Statement) (Facts, error) {
	rel := st.IndexRelation()
	// One row for an index: its table, and the primary-key, unique, or
	// exclusion constraint it backs, if any. A relation that is not an
	// index has no pg_index row, so the lookup finds nothing.
	//
	// INV: CO-9 — every function, operator, relation, and type is
	// pg_catalog-qualified, so a planted overload on the session's
	// search_path cannot change which table the facts describe.
	// to_regclass itself still resolves the bare name on that path,
	// exactly as the statement will.
	const q = `
		SELECT tn.nspname, t.relname,
		       COALESCE((
		           SELECT con.conname
		           FROM pg_catalog.pg_constraint con
		           WHERE con.conindid OPERATOR(pg_catalog.=) i.indexrelid
		             AND con.contype OPERATOR(pg_catalog.=) ANY (ARRAY['p', 'u', 'x']::pg_catalog."char"[])), '')
		FROM pg_catalog.pg_index i
		JOIN pg_catalog.pg_class t ON t.oid OPERATOR(pg_catalog.=) i.indrelid
		JOIN pg_catalog.pg_namespace tn ON tn.oid OPERATOR(pg_catalog.=) t.relnamespace
		WHERE i.indexrelid OPERATOR(pg_catalog.=) pg_catalog.to_regclass($1)`
	name := regclassName(rel)
	var f Facts
	err := pool.QueryRow(ctx, q, name).Scan(&f.Schema, &f.Table, &f.Classifier.RenamedIndexConstraint)
	if errors.Is(err, pgx.ErrNoRows) {
		return Facts{}, fmt.Errorf("%w: %s; nothing was executed", ErrRenamedIndexNotFound, name)
	}
	if err != nil {
		return Facts{}, fmt.Errorf("resolve the table that owns index %s: %w", name, err)
	}
	if f.Target, err = preflight.LookupTargetFacts(ctx, pool, f.Schema, f.Table); err != nil {
		return Facts{}, err
	}
	exists := true
	f.TableExists = &exists
	return f, nil
}

// renameIndex is Run for an ALTER INDEX ... RENAME TO. The statement names
// an index, not a table, so the table every later stage acts on — the
// plan's facts, preflight, the privilege check, and the executor's lock —
// is the one that owns the index, read from the catalog. A plain index
// rename runs natively; a rename of an index that backs a constraint
// renames the constraint and is refused as rewrite-required.
func renameIndex(ctx context.Context, pool *pgxpool.Pool, st statement.Statement, opts Options, logger *slog.Logger) (verdict.Verdict, error) {
	if opts.Force != "" {
		return verdict.Verdict{}, fmt.Errorf("%w: force names a table, and this statement names an index; nothing was executed",
			ErrIndexRenameAcknowledgement)
	}
	if opts.CopySwap.Restart != "" {
		return verdict.Verdict{}, fmt.Errorf("%w: an index rename never runs through copy-and-swap; nothing was executed",
			ErrIndexRenameAcknowledgement)
	}
	if opts.CopySwap.BlockWrites != "" {
		return verdict.Verdict{}, fmt.Errorf("%w: block-writes names a table to fence, and an index rename never runs through copy-and-swap; nothing was executed",
			ErrIndexRenameAcknowledgement)
	}
	facts, err := LiveFacts(ctx, pool, st)
	if err != nil {
		return verdict.Verdict{}, err
	}
	canonical, err := statement.Canonical(st.SQL())
	if err != nil {
		return verdict.Verdict{}, err
	}
	classified, err := planner.Classify(canonical, facts.Classifier)
	if err != nil {
		return verdict.Verdict{}, err
	}
	rs := router.Route([]planner.Plan{classified}).Statements[0]
	table := facts.Schema + "." + facts.Table
	logger.Debug("index rename routed",
		"table", table, "route", string(classified.Route), "disposition", string(rs.Disposition))

	switch rs.Disposition {
	case router.DispositionExecute:
		return executeIndexRename(ctx, pool, st, facts, rs.Plan, opts, logger)
	case router.DispositionRewriteRequired:
		v, err := rewriteRequiredVerdict(st, rs)
		v.Table = table
		return v, err
	default:
		// The planner routes an index rename native, as metadata-only or
		// as an app-breaking rename; any other disposition is a
		// classification this path was not written for.
		return verdict.Verdict{}, fmt.Errorf("%w: index rename routed to disposition %q",
			executor.ErrInvariantViolation, rs.Disposition)
	}
}

// executeIndexRename preflights the table that owns the renamed index,
// checks the connected role at the tier the rename needs, and runs the
// rename as one bounded attempt, which re-checks under the table's lock
// that the name still resolves to a plain index of that table.
func executeIndexRename(ctx context.Context, pool *pgxpool.Pool, st statement.Statement, facts Facts,
	plan planner.Plan, opts Options, logger *slog.Logger) (verdict.Verdict, error) {
	table := facts.Schema + "." + facts.Table
	limit := opts.MaxTableSizeBytes
	if !sizeGuardApplies(plan, false) {
		limit = preflight.NoSizeLimit
	}
	pt, err := preflight.CheckTable(ctx, pool, facts.Schema, facts.Table, limit)
	var sizeErr *preflight.SizeError
	if errors.As(err, &sizeErr) {
		v := sizeGuardVerdict(st, sizeErr, false)
		v.Table = table
		return v, nil
	}
	if err != nil {
		return verdict.Verdict{}, err
	}
	tier, err := preflight.RequiredTier([]string{st.SQL()})
	if err != nil {
		return verdict.Verdict{}, err
	}
	_, err = preflight.CheckPrivileges(ctx, pool, facts.Schema, facts.Table, preflight.Requirement{Tier: tier})
	var privErr *preflight.PrivilegeError
	if errors.As(err, &privErr) {
		v := privilegeVerdict(st, privErr, false)
		v.Table = table
		return v, nil
	}
	if err != nil {
		return verdict.Verdict{}, err
	}

	start := time.Now()
	err = executor.ExecuteIndexRename(ctx, pool, pt, st, opts.Budget.Brief, opts.retry())
	elapsed := time.Since(start)
	if v, refused := execRefusal(st, err, false, false, false); refused {
		v.Table = table
		logger.Debug("index rename refused",
			"table", table, "reason", string(v.Reason), "cause", string(v.Cause), "elapsed", elapsed)
		return v, nil
	}
	if err != nil {
		v := failureVerdict(st, err, executor.SequenceReport{}, false)
		v.Table = table
		logger.Debug("index rename failed", "table", table, "code", v.Code, "elapsed", elapsed)
		return v, fmt.Errorf("run index rename on %s: %w", table, err)
	}
	logger.Debug("index rename committed", "table", table, "elapsed", elapsed)
	return verdict.Verdict{
		Outcome:   verdict.OutcomeExecuted,
		Statement: st.SQL(),
		Table:     table,
		Detail: fmt.Sprintf("committed within budgets (lock %s, statement %s): the rename was effectively instant",
			opts.Budget.Brief.LockTimeout, opts.Budget.Brief.StatementTimeout),
	}, nil
}
