package migrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// ErrEnumValueAcknowledgement means an acknowledgement was given for an
// ALTER TYPE ... ADD VALUE, which none of the table acknowledgements apply
// to. Nothing has executed.
var ErrEnumValueAcknowledgement = errors.New("the acknowledgement does not apply to ALTER TYPE ... ADD VALUE")

// enumTypeFacts is LiveFacts for an ALTER TYPE ... ADD VALUE: the statement
// names a type that no table owns, so the facts carry the type's schema and
// no table.
func enumTypeFacts(ctx context.Context, pool *pgxpool.Pool, st statement.Statement) (Facts, error) {
	schema, err := preflight.LookupEnumSchema(ctx, pool, st.EnumType())
	if err != nil {
		return Facts{}, err
	}
	return Facts{Schema: schema}, nil
}

// addEnumValue is Run for an ALTER TYPE ... ADD VALUE. The statement names
// an enum type and locks only that type, so no table is preflighted: the
// type is resolved and the role's right to alter it checked
// (preflight.CheckEnumTarget), and the statement runs as one bounded
// attempt that confirms before the commit that the label went to that
// type.
func addEnumValue(ctx context.Context, pool *pgxpool.Pool, st statement.Statement, opts Options, logger *slog.Logger) (verdict.Verdict, error) {
	if err := rejectTableAcks(opts); err != nil {
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
		"route", string(classified.Route), "disposition", string(rs.Disposition))
	if rs.Disposition != router.DispositionExecute {
		// The planner routes ADD VALUE native as metadata-only; any other
		// disposition is a classification this path was not written for.
		return verdict.Verdict{}, fmt.Errorf("%w: enum value add routed to disposition %q",
			executor.ErrInvariantViolation, rs.Disposition)
	}

	et, err := preflight.CheckEnumTarget(ctx, pool, st.EnumType())
	if privErr, ok := errors.AsType[*preflight.PrivilegeError](err); ok {
		return privilegeVerdict(st, privErr, false), nil
	}
	if err != nil {
		return verdict.Verdict{}, err
	}

	start := time.Now()
	err = executor.ExecuteEnumValueAdd(ctx, pool, et, st, opts.Budget.Brief, opts.retry())
	elapsed := time.Since(start)
	if v, refused := execRefusal(st, err, false, false, false); refused {
		logger.Debug("enum value add refused",
			"type", et.Qualified(), "reason", string(v.Reason), "cause", string(v.Cause), "elapsed", elapsed)
		return v, nil
	}
	if err != nil {
		v := failureVerdict(st, err, executor.SequenceReport{}, false)
		logger.Debug("enum value add failed", "type", et.Qualified(), "code", v.Code, "elapsed", elapsed)
		return v, fmt.Errorf("add a value to enum type %s: %w", et.Qualified(), err)
	}
	logger.Debug("enum value add committed", "type", et.Qualified(), "elapsed", elapsed)
	return verdict.Verdict{
		Outcome:   verdict.OutcomeExecuted,
		Statement: st.SQL(),
		Detail: fmt.Sprintf("committed within budgets (lock %s, statement %s): %s carries the value, and no table was locked",
			opts.Budget.Brief.LockTimeout, opts.Budget.Brief.StatementTimeout, et.Qualified()),
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
