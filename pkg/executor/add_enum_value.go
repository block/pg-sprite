package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/statement"
)

// ErrEnumTypeDrift means an ALTER TYPE ... ADD VALUE changed a type other
// than the enum type the engine resolved and checked: another session moved
// the type's name to a different type before the statement ran, so the
// name reached that type instead. Nothing was added. The outcome is
// permanent: plan again against the catalog as it stands.
var ErrEnumTypeDrift = errors.New("the ADD VALUE reached a type other than the enum type the engine checked")

// ExecuteEnumValueAdd runs an ALTER TYPE ... ADD VALUE as bounded attempts
// under transaction-local budgets, retrying only lock_timeout failures,
// bounded by retry. The statement locks only the type, never a table that
// uses it, and waits only on another session altering the same type.
//
// The statement names a type, not a table, so the ST-7 binding is checked
// in the catalog against typ, the OID of the enum type in schema the
// engine resolved and checked the role's ownership of. Each attempt pins
// its search_path to schema alone, so a bare name resolves only there.
// The type's lock does not hold its name still, since ALTER TYPE ...
// RENAME does not wait for it, so after the statement runs, and before the
// commit, the attempt confirms that every enum label the transaction wrote
// belongs to typ and that typ carries the label, and rolls back with
// ErrEnumTypeDrift otherwise.
func ExecuteEnumValueAdd(ctx context.Context, pool *pgxpool.Pool, st statement.Statement, schema string, typ uint32, b Budget, retry RetryPolicy) error {
	if err := b.validate(); err != nil {
		return err
	}
	if err := retry.validate(); err != nil {
		return err
	}
	if st.Kind() != statement.KindAddEnumValue {
		return fmt.Errorf("%w: ExecuteEnumValueAdd runs ALTER TYPE ... ADD VALUE, got %s", ErrInvariantViolation, st.Kind())
	}
	return executeWithLockRetry(ctx, retry, func(ctx context.Context) error {
		// INV: ST-7 — the label is added only to the enum type the engine
		// checked, confirmed by identity before the commit.
		after := func(ctx context.Context, tx pgx.Tx) error {
			return refuseEnumTypeDrift(ctx, tx, st, typ)
		}
		return executeBoundedAttemptAs(ctx, pool, st, b, []string{schema}, "", nil, after)
	}, sleepContext)
}

// refuseEnumTypeDrift runs after the ADD VALUE, in its transaction, and
// refuses unless typ carries the added label and no other type has a label
// this transaction wrote. A row written by this transaction carries its
// transaction ID as xmin; pg_current_xact_id adds the epoch above the
// 32-bit ID that xmin stores, so the comparison drops it. The statement
// writes no row when IF NOT EXISTS finds the label present, and the type
// that carries the label is then still typ.
//
// INV: CO-9 — every function, operator, and relation is
// pg_catalog-qualified, so a planted overload on the search_path cannot
// change which rows the check reads.
func refuseEnumTypeDrift(ctx context.Context, tx pgx.Tx, st statement.Statement, typ uint32) error {
	const q = `
		SELECT EXISTS (
		           SELECT FROM pg_catalog.pg_enum e
		           WHERE e.enumtypid OPERATOR(pg_catalog.=) $1
		             AND e.enumlabel OPERATOR(pg_catalog.=) $2),
		       EXISTS (
		           SELECT FROM pg_catalog.pg_enum e
		           WHERE e.enumtypid OPERATOR(pg_catalog.<>) $1
		             AND e.xmin::pg_catalog.text::pg_catalog.int8 OPERATOR(pg_catalog.=)
		                 (pg_catalog.pg_current_xact_id()::pg_catalog.text::pg_catalog.int8 OPERATOR(pg_catalog.%) 4294967296))`
	var labeled, otherChanged bool
	if err := tx.QueryRow(ctx, q, typ, st.AddedEnumValue()).Scan(&labeled, &otherChanged); err != nil {
		return fmt.Errorf("confirm the ADD VALUE changed only enum type OID %d: %w", typ, err)
	}
	if otherChanged {
		return fmt.Errorf("%w: another session gave the name to a different type before the statement reached it; nothing was added",
			ErrEnumTypeDrift)
	}
	if !labeled {
		return fmt.Errorf("%w: the checked type does not carry the label after the statement; nothing was added", ErrEnumTypeDrift)
	}
	return nil
}
