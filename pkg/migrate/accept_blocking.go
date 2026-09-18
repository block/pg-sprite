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
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

var (
	// ErrAcceptBlockingMismatch means the acknowledgement named a table other
	// than the one the accepted statement locks. Nothing has executed.
	ErrAcceptBlockingMismatch = errors.New("the accept-blocking acknowledgement must name the table the statement locks")
	// ErrAcceptBlockingRelationNotFound means the index or table the
	// accepted statement names does not exist or is not visible on the
	// session search_path, so there is no table to acknowledge. Nothing has
	// executed.
	ErrAcceptBlockingRelationNotFound = errors.New("the accepted statement names a relation that does not exist")
	// ErrAcceptBlockingWrongRelationKind means the accepted statement names a
	// relation that exists but is not what the statement operates on — a
	// table where DROP INDEX / REINDEX INDEX needs an index, or an index
	// where REINDEX TABLE needs a table — so there is no lock to
	// acknowledge. Nothing has executed.
	ErrAcceptBlockingWrongRelationKind = errors.New("the accepted statement names a relation of the wrong kind")
)

// AcceptedRefusal returns the refusal's in-process proof when the
// acknowledgement applies to it: the caller supplied one and the proof is
// in the eligible set. Ineligible refusals — and a verdict whose proof does
// not validate, which this build's gate cannot produce — stand exactly as
// they would without the acknowledgement. [Run] makes this decision itself;
// it is exported so a front door that refuses gated statements before
// dialing can tell the one gate refusal that needs the database.
func AcceptedRefusal(ack string, v verdict.Verdict) (verdict.Refusal, bool) {
	if ack == "" {
		return verdict.Refusal{}, false
	}
	proof, err := v.Refusal()
	if err != nil {
		return verdict.Refusal{}, false
	}
	return proof, verdict.AcceptedBlockingEligible(proof)
}

// acceptBlocking runs a gate-refused single-relation index statement as-is
// under engine-owned lock and statement budgets, because the operator
// acknowledged the lock it takes. The refusal has already been produced and
// its proof found eligible; this resolves the table the statement locks, checks the
// acknowledgement names it, records the audit event, executes, and maps the
// outcome: a commit is the without-online-safety outcome carrying the
// accepted refusal's identity, an exhausted lock budget is a refusal (the
// statement never ran), a cancelled or failed statement is a failure, and a
// statement the executor or server will not run on this path is refused as
// unsupported.
func acceptBlocking(ctx context.Context, pool *pgxpool.Pool, st statement.Statement,
	refused verdict.Verdict, proof verdict.Refusal, opts Options) (verdict.Verdict, error) {
	table, err := lockedTable(ctx, pool, st.IndexRelation())
	if err != nil {
		return verdict.Verdict{}, err
	}
	if table != opts.AcceptBlocking {
		return verdict.Verdict{}, fmt.Errorf("%w: %s locks %q, got %q; nothing was executed",
			ErrAcceptBlockingMismatch, st.Kind(), table, opts.AcceptBlocking)
	}
	budget := executor.BlockingBudget{
		LockTimeout:      opts.Budget.Brief.LockTimeout,
		StatementTimeout: opts.Budget.Brief.StatementTimeout,
	}
	auditAcceptBlocking(opts.audit(), st, table, proof, budget)

	start := time.Now()
	_, err = executor.ExecuteAcceptedBlocking(ctx, pool, st.SQL(), budget)
	elapsed := time.Since(start)
	logger := opts.logger()
	if errors.Is(err, executor.ErrInvalidBlockingBudget) {
		return verdict.Verdict{}, err
	}
	if errors.Is(err, executor.ErrUnsupportedAcceptedBlocking) {
		// Decided before or instead of execution, and permanent: the same
		// statement reproduces it every time.
		v := admissionRefusalVerdict(st, err, verdict.CapabilityBoundary(verdict.ReasonUnsupportedStatement), false)
		v.Table = table
		return v, nil
	}
	var budgetErr *executor.BudgetError
	if errors.As(err, &budgetErr) && budgetErr.Cause == executor.CauseLock {
		// INV: AB-2 — the lock was never granted, so the statement never
		// ran; the refusal contract applies.
		v := budgetVerdict(st, budgetErr, false, false)
		v.Table = table
		logger.Debug("accepted blocking execution refused",
			"table", table, "reason", string(v.Reason), "cause", string(v.Cause), "elapsed", elapsed)
		return v, nil
	}
	if err != nil {
		// A statement-budget cancellation is a failure on this path, not a
		// refusal: the lock was granted and the statement was doing the
		// work the operator accepted when the bound cut it off.
		v := failureVerdict(st, err, executor.SequenceReport{}, false)
		v.Table = table
		var unknownErr *executor.BlockingOutcomeUnknownError
		if errors.As(err, &unknownErr) {
			// The client lost the transaction at or after submission, so the
			// verdict cannot claim the rollback a failed attempt normally
			// gets: the statement may have committed.
			v.Detail = fmt.Sprintf("outcome unknown: the connection or the caller's context ended after %s was submitted, "+
				"so whether it committed cannot be established from the client — inspect the catalog for %s before retrying",
				st.Kind(), table)
		}
		logger.Debug("accepted blocking execution failed",
			"table", table, "code", v.Code, "elapsed", elapsed)
		return v, fmt.Errorf("run accepted blocking statement on %s: %w", table, err)
	}
	logger.Debug("accepted blocking statement committed", "table", table, "elapsed", elapsed)

	v, err := refused.WithAcceptedBlocking(proof, budget.LockTimeout, budget.StatementTimeout)
	if err != nil {
		return verdict.Verdict{}, fmt.Errorf("%w: %w", executor.ErrInvariantViolation, err)
	}
	v.Table = table
	v.Detail = fmt.Sprintf("the operator accepted the blocking form: it ran as-is under budgets (lock %s, statement %s) "+
		"with no online-safety guarantee; %s was locked for the statement's duration",
		budget.LockTimeout, budget.StatementTimeout, table)
	return v, nil
}

// lockedTable resolves the table an accepted single-relation index statement
// locks: the owning table of a named index (DROP INDEX, REINDEX INDEX),
// read from the catalog, or the named table itself (REINDEX TABLE). An
// unqualified name resolves against the session search_path, exactly as
// the statement will when it runs on the same pool. A name that resolves
// to the wrong kind of relation is its own error, so an operator who typed
// a table where the statement needs an index is pointed at the statement,
// not at search_path and grants.
func lockedTable(ctx context.Context, pool *pgxpool.Pool, rel statement.IndexRelation) (string, error) {
	if rel.Kind != statement.IndexRelationIndex && rel.Kind != statement.IndexRelationTable {
		return "", fmt.Errorf("%w: accepted statement names no single relation", executor.ErrInvariantViolation)
	}
	// One row per resolved relation: its kind, and — when it is an index —
	// the table that owns it; for a table the owner columns repeat the
	// relation itself, so the result is always the table the statement locks.
	const q = `
		SELECT c.relkind::text,
		       COALESCE(tn.nspname, n.nspname),
		       COALESCE(t.relname, c.relname)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_index i ON i.indexrelid = c.oid
		LEFT JOIN pg_class t ON t.oid = i.indrelid
		LEFT JOIN pg_namespace tn ON tn.oid = t.relnamespace
		WHERE c.oid = to_regclass($1)`
	name := regclassName(rel)
	var relkind, schema, table string
	err := pool.QueryRow(ctx, q, name).Scan(&relkind, &schema, &table)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: %s is not visible on the session search_path; nothing was executed",
			ErrAcceptBlockingRelationNotFound, name)
	}
	if err != nil {
		return "", fmt.Errorf("resolve the table %s locks: %w", name, err)
	}
	if !relationKindMatches(rel.Kind, relkind) {
		return "", fmt.Errorf("%w: %s is %s, and the statement needs %s; nothing was executed",
			ErrAcceptBlockingWrongRelationKind, name, describeRelkind(relkind), describeIndexRelationKind(rel.Kind))
	}
	return schema + "." + table, nil
}

// relationKindMatches reports whether a resolved pg_class.relkind is what
// the accepted statement operates on: an index (plain or partitioned) for
// DROP INDEX / REINDEX INDEX; a table, partitioned table, or materialized
// view for REINDEX TABLE, which is the set PostgreSQL itself accepts there.
func relationKindMatches(kind statement.IndexRelationKind, relkind string) bool {
	switch kind {
	case statement.IndexRelationIndex:
		return relkind == "i" || relkind == "I"
	case statement.IndexRelationTable:
		return relkind == "r" || relkind == "p" || relkind == "m"
	default:
		return false
	}
}

// describeRelkind names a pg_class.relkind for an operator-facing message.
func describeRelkind(relkind string) string {
	switch relkind {
	case "r":
		return "a table"
	case "p":
		return "a partitioned table"
	case "i":
		return "an index"
	case "I":
		return "a partitioned index"
	case "m":
		return "a materialized view"
	case "v":
		return "a view"
	case "S":
		return "a sequence"
	case "f":
		return "a foreign table"
	case "c":
		return "a composite type"
	case "t":
		return "a TOAST table"
	default:
		return fmt.Sprintf("a relation of kind %q", relkind)
	}
}

// describeIndexRelationKind names what the accepted statement operates on,
// for the same message.
func describeIndexRelationKind(kind statement.IndexRelationKind) string {
	if kind == statement.IndexRelationIndex {
		return "an index"
	}
	return "a table"
}

// regclassName renders the relation as the quoted text to_regclass parses,
// so a mixed-case or reserved-word name resolves as spelled.
func regclassName(rel statement.IndexRelation) string {
	if rel.Schema == "" {
		return pgx.Identifier{rel.Name}.Sanitize()
	}
	return pgx.Identifier{rel.Schema, rel.Name}.Sanitize()
}

// auditAcceptBlocking records the acknowledgement before anything executes:
// the operator chose to run a refused blocking statement under budgets. Like
// the force audit it is warn-level and unconditional, and the verdict's
// BlockingPassthrough field is its machine-readable twin on a commit.
func auditAcceptBlocking(audit *slog.Logger, st statement.Statement, table string,
	proof verdict.Refusal, budget executor.BlockingBudget) {
	audit.Warn("accepted blocking execution of refused statement",
		"table", table,
		"kind", st.Kind().String(),
		"reason", string(proof.Reason()),
		"class", string(proof.Class()),
		"lock_timeout", budget.LockTimeout,
		"statement_timeout", budget.StatementTimeout)
}
