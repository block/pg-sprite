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
	// ErrEnumTypeNotFound means the type an ALTER TYPE ... ADD VALUE names
	// is not an enum type visible on the session search_path. Nothing has
	// executed.
	ErrEnumTypeNotFound = errors.New("the type ADD VALUE names is not an enum type visible on the session search_path")
	// ErrEnumValueAcknowledgement means an acknowledgement was given for an
	// ALTER TYPE ... ADD VALUE, which none of the table acknowledgements
	// apply to. Nothing has executed.
	ErrEnumValueAcknowledgement = errors.New("the acknowledgement does not apply to ALTER TYPE ... ADD VALUE")
)

// enumType is the enum type an ALTER TYPE ... ADD VALUE names, resolved
// from the catalog.
type enumType struct {
	oid    uint32
	schema string
	name   string
}

// qualified returns the type's schema-qualified name for operator-facing
// text.
func (e enumType) qualified() string { return e.schema + "." + e.name }

// resolveEnumType reads the enum type the statement names. An unqualified
// name resolves on the session search_path, exactly as the statement will
// when it runs on the same pool.
// A qualified name in a schema the role lacks USAGE on is the
// *preflight.PrivilegeError that carries the grant.
func resolveEnumType(ctx context.Context, pool *pgxpool.Pool, st statement.Statement) (enumType, error) {
	ref := st.EnumType()
	name := pgx.Identifier{ref.Name}
	if ref.Schema != "" {
		name = pgx.Identifier{ref.Schema, ref.Name}
		if err := preflight.CheckTypeSchemaUsage(ctx, pool, ref.Schema); err != nil {
			return enumType{}, err
		}
	}
	// INV: CO-9 — every function, operator, relation, and type is
	// pg_catalog-qualified, so a planted overload on the session's
	// search_path cannot change which type the facts describe.
	// to_regtype itself still resolves the bare name on that path,
	// exactly as the statement will.
	const q = `
		SELECT t.oid, n.nspname, t.typname, t.typtype OPERATOR(pg_catalog.=) 'e'
		FROM pg_catalog.pg_type t
		JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) t.typnamespace
		WHERE t.oid OPERATOR(pg_catalog.=) pg_catalog.to_regtype($1)`
	var e enumType
	var isEnum bool
	err := pool.QueryRow(ctx, q, name.Sanitize()).Scan(&e.oid, &e.schema, &e.name, &isEnum)
	if errors.Is(err, pgx.ErrNoRows) {
		return enumType{}, fmt.Errorf("%w: no type %s; nothing was executed", ErrEnumTypeNotFound, name.Sanitize())
	}
	if err != nil {
		return enumType{}, fmt.Errorf("resolve enum type %s: %w", name.Sanitize(), err)
	}
	if !isEnum {
		return enumType{}, fmt.Errorf("%w: %s is not an enum type; nothing was executed", ErrEnumTypeNotFound, e.qualified())
	}
	return e, nil
}

// enumTypeFacts is LiveFacts for an ALTER TYPE ... ADD VALUE: the statement
// names a type that no table owns, so the facts carry the type's schema and
// no table.
func enumTypeFacts(ctx context.Context, pool *pgxpool.Pool, st statement.Statement) (Facts, error) {
	e, err := resolveEnumType(ctx, pool, st)
	if err != nil {
		return Facts{}, err
	}
	return Facts{Schema: e.schema}, nil
}

// addEnumValue is Run for an ALTER TYPE ... ADD VALUE. The statement names
// an enum type and locks only that type, so no table is preflighted:
// the role's ownership of the type is checked, and the statement runs as
// one bounded attempt that confirms before the commit that the label went
// to that type.
func addEnumValue(ctx context.Context, pool *pgxpool.Pool, st statement.Statement, opts Options, logger *slog.Logger) (verdict.Verdict, error) {
	if err := rejectTableAcks(opts); err != nil {
		return verdict.Verdict{}, err
	}
	e, err := resolveEnumType(ctx, pool, st)
	var privErr *preflight.PrivilegeError
	if errors.As(err, &privErr) {
		return privilegeVerdict(st, privErr, false), nil
	}
	if err != nil {
		return verdict.Verdict{}, err
	}
	canonical, err := statement.Canonical(st.SQL())
	if err != nil {
		return verdict.Verdict{}, err
	}
	classified, err := planner.Classify(canonical, planner.Facts{})
	if err != nil {
		return verdict.Verdict{}, err
	}
	rs := router.Route([]planner.Plan{classified}).Statements[0]
	logger.Debug("enum value add routed",
		"type", e.qualified(), "route", string(classified.Route), "disposition", string(rs.Disposition))
	if rs.Disposition != router.DispositionExecute {
		// The planner routes ADD VALUE native as metadata-only; any other
		// disposition is a classification this path was not written for.
		return verdict.Verdict{}, fmt.Errorf("%w: enum value add routed to disposition %q",
			executor.ErrInvariantViolation, rs.Disposition)
	}

	err = preflight.CheckTypeOwnership(ctx, pool, e.oid)
	if errors.As(err, &privErr) {
		return privilegeVerdict(st, privErr, false), nil
	}
	if err != nil {
		return verdict.Verdict{}, err
	}

	start := time.Now()
	err = executor.ExecuteEnumValueAdd(ctx, pool, st, e.schema, e.oid, opts.Budget.Brief, opts.retry())
	elapsed := time.Since(start)
	if v, refused := execRefusal(st, err, false, false, false); refused {
		logger.Debug("enum value add refused",
			"type", e.qualified(), "reason", string(v.Reason), "cause", string(v.Cause), "elapsed", elapsed)
		return v, nil
	}
	if err != nil {
		v := failureVerdict(st, err, executor.SequenceReport{}, false)
		logger.Debug("enum value add failed", "type", e.qualified(), "code", v.Code, "elapsed", elapsed)
		return v, fmt.Errorf("add a value to enum type %s: %w", e.qualified(), err)
	}
	logger.Debug("enum value add committed", "type", e.qualified(), "elapsed", elapsed)
	return verdict.Verdict{
		Outcome:   verdict.OutcomeExecuted,
		Statement: st.SQL(),
		Detail: fmt.Sprintf("committed within budgets (lock %s, statement %s): %s carries the value, and no table was locked",
			opts.Budget.Brief.LockTimeout, opts.Budget.Brief.StatementTimeout, e.qualified()),
	}, nil
}

// rejectTableAcks refuses every acknowledgement that names a table for an
// ALTER TYPE ... ADD VALUE, which names a type, never runs through
// copy-and-swap, and discards nothing.
func rejectTableAcks(opts Options) error {
	switch {
	case opts.Force != "":
		return fmt.Errorf("%w: force names a table, and this statement names a type; nothing was executed",
			ErrEnumValueAcknowledgement)
	case opts.CopySwap.Restart != "":
		return fmt.Errorf("%w: adding an enum value never runs through copy-and-swap; nothing was executed",
			ErrEnumValueAcknowledgement)
	case opts.CopySwap.BlockWrites != "":
		return fmt.Errorf("%w: block-writes names a table to fence, and adding an enum value never runs through copy-and-swap; nothing was executed",
			ErrEnumValueAcknowledgement)
	case opts.AllowDestructive != "":
		return fmt.Errorf("%w: allow-destructive names a table, and adding an enum value discards nothing; nothing was executed",
			ErrEnumValueAcknowledgement)
	}
	return nil
}
