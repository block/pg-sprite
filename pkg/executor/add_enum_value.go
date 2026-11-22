package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/statement"
)

// ErrEnumTypeDrift means an ALTER TYPE ... ADD VALUE changed a type other
// than the enum type the engine resolved and checked: another session moved
// the type's name to a different type before the statement ran, so the
// name reached that type instead. Nothing was added. The outcome is
// permanent: plan again against the catalog as it stands.
var ErrEnumTypeDrift = errors.New("the ADD VALUE reached a type other than the enum type the engine checked")

// ErrEnumLabelExists means the enum type the engine checked already carries
// the label an ALTER TYPE ... ADD VALUE without IF NOT EXISTS adds: another
// change added it after the statement was planned. Nothing was added. The
// outcome is permanent: the same statement meets the same label on every
// attempt, so plan again against the catalog as it stands.
var ErrEnumLabelExists = errors.New("the enum type already carries the label the ADD VALUE adds")

// ErrEnumNeighborMissing means the enum type the engine checked no longer
// carries the label an ALTER TYPE ... ADD VALUE places its value BEFORE or
// AFTER: another change renamed or removed it after the statement was
// planned. Nothing was added. The outcome is permanent: the same statement
// meets the same missing label on every attempt, so plan again against the
// catalog as it stands.
var ErrEnumNeighborMissing = errors.New("the enum type no longer carries the label the ADD VALUE places its value next to")

// sqlstateInvalidParameterValue is the server's invalid_parameter_value,
// which an ADD VALUE raises when the label its BEFORE or AFTER names is not
// on the type. A label the type already carries raises duplicate_object.
const sqlstateInvalidParameterValue = "22023"

// ExecuteEnumValueAdd runs an ALTER TYPE ... ADD VALUE as bounded attempts
// under transaction-local budgets, retrying only lock_timeout failures,
// bounded by retry. The statement locks only the type, never a table that
// uses it, and waits only on another session altering the same type.
//
// It accepts only an EnumTarget minted for the type name the statement
// spells, and pins each attempt's search_path to the target's schema
// alone, so a bare name resolves only there. The statement names a type,
// not a table, so the ST-7 binding is checked in the catalog against the
// target's OID. The type's lock does not hold its name still, since ALTER
// TYPE ... RENAME does not wait for it, so after the statement runs, and
// before the commit, the attempt confirms that every enum label the
// transaction wrote belongs to the target, that the target carries the
// label, and, when the statement wrote nothing, that the name still
// resolves to the target, and rolls back with ErrEnumTypeDrift otherwise.
//
// A statement the server rejects because the type already carries the
// label, or lacks the label BEFORE or AFTER names, returns
// ErrEnumLabelExists or ErrEnumNeighborMissing when the target, read by
// OID, shows that fact, and ErrEnumTypeDrift when it does not, since the
// statement then reached a different type.
func ExecuteEnumValueAdd(ctx context.Context, pool *pgxpool.Pool, et preflight.EnumTarget, st statement.Statement, b Budget, retry RetryPolicy) error {
	if err := b.validate(); err != nil {
		return err
	}
	if err := retry.validate(); err != nil {
		return err
	}
	if st.Kind() != statement.KindAddEnumValue {
		return fmt.Errorf("%w: ExecuteEnumValueAdd runs ALTER TYPE ... ADD VALUE, got %s", ErrInvariantViolation, st.Kind())
	}
	// INV: ST-7 — the proof was minted for the name this statement spells.
	if et.OID() == 0 || et.Ref() != st.EnumType() {
		return fmt.Errorf("%w: the enum target was not checked for the type %s names", ErrInvariantViolation, st.SQL())
	}
	err := executeWithLockRetry(ctx, retry, func(ctx context.Context) error {
		// INV: ST-7 — the label is added only to the enum type the engine
		// checked, confirmed by identity before the commit.
		after := func(ctx context.Context, tx pgx.Tx) error {
			return refuseEnumTypeDrift(ctx, tx, st, et.OID())
		}
		return executeBoundedAttemptAs(ctx, pool, st, b, []string{et.Schema()}, "", nil, after)
	}, sleepContext)
	return classifyEnumValueAddRejection(ctx, pool, et, st, b, err)
}

// classifyEnumValueAddRejection turns the server's rejection of an ADD
// VALUE for a label already present, or for a missing BEFORE or AFTER
// label, into its typed outcome. The rejection names no type, and another
// session can give the statement's name to a different type before it
// runs, so the fact is read from the checked type by OID: when the target
// shows it, the outcome is ErrEnumLabelExists or ErrEnumNeighborMissing;
// when it does not, the statement reached another type, which is
// ErrEnumTypeDrift. Any other error is returned as it came.
//
// The read runs after the attempt rolled back, outside the transaction its
// budget bounded, so it gets a deadline of its own, the statement timeout,
// covering both the wait for a pooled session and the catalog read.
func classifyEnumValueAddRejection(ctx context.Context, pool *pgxpool.Pool, et preflight.EnumTarget, st statement.Statement, b Budget, err error) error {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	if !ok {
		return err
	}
	var label string
	var wantPresent bool
	var outcome error
	switch {
	case pgErr.Code == sqlstateDuplicateObject:
		label, wantPresent, outcome = st.AddedEnumValue(), true, ErrEnumLabelExists
	case pgErr.Code == sqlstateInvalidParameterValue && st.EnumValueNeighbor() != "":
		label, wantPresent, outcome = st.EnumValueNeighbor(), false, ErrEnumNeighborMissing
	default:
		return err
	}
	readCtx, cancel := context.WithTimeout(ctx, b.StatementTimeout)
	defer cancel()
	present, readErr := enumCarriesLabel(readCtx, pool, et.OID(), label)
	if readErr != nil {
		return fmt.Errorf("read enum type OID %d for label %q after the server rejected the ADD VALUE (%w): %w", et.OID(), label, err, readErr)
	}
	if present != wantPresent {
		return fmt.Errorf("%w: the server rejected the statement over label %q, but the checked type does not show why, so the statement reached another type; nothing was added: %w",
			ErrEnumTypeDrift, label, err)
	}
	return fmt.Errorf("%w: label %q on %s; nothing was added: %w", outcome, label, et.Qualified(), err)
}

// enumCarriesLabel reports whether the enum type with OID typ carries label.
//
// INV: CO-9 — the relation and operators are pg_catalog-qualified, so a
// planted object on the search_path cannot change which rows the read sees.
func enumCarriesLabel(ctx context.Context, pool *pgxpool.Pool, typ uint32, label string) (bool, error) {
	const q = `
		SELECT EXISTS (
		    SELECT FROM pg_catalog.pg_enum e
		    WHERE e.enumtypid OPERATOR(pg_catalog.=) $1
		      AND e.enumlabel OPERATOR(pg_catalog.=) $2)`
	var present bool
	if err := pool.QueryRow(ctx, q, typ, label).Scan(&present); err != nil {
		return false, fmt.Errorf("read the labels of enum type OID %d: %w", typ, err)
	}
	return present, nil
}

// refuseEnumTypeDrift runs after the ADD VALUE, in its transaction, and
// refuses unless typ carries the added label and no other type has a label
// this transaction wrote. A row written by this transaction carries its
// transaction ID as xmin; pg_current_xact_id adds the epoch above the
// 32-bit ID that xmin stores, so the comparison drops it. When IF NOT
// EXISTS finds the label present the statement writes no row, so no row
// identifies the type it reached: the name is resolved again, as the
// statement resolved it, and must still be typ. A rename that lands
// between the statement and this read refuses a statement that did reach
// typ, which fails closed with nothing changed.
//
// INV: CO-9 — every function, operator, and relation is
// pg_catalog-qualified, so a planted overload on the search_path cannot
// change which rows the check reads. to_regtype itself still resolves the
// bare name on that path, exactly as the statement did.
func refuseEnumTypeDrift(ctx context.Context, tx pgx.Tx, st statement.Statement, typ uint32) error {
	const q = `
		WITH own AS (
		    SELECT e.enumtypid
		    FROM pg_catalog.pg_enum e
		    WHERE e.xmin::pg_catalog.text::pg_catalog.int8 OPERATOR(pg_catalog.=)
		          (pg_catalog.pg_current_xact_id()::pg_catalog.text::pg_catalog.int8 OPERATOR(pg_catalog.%) 4294967296))
		SELECT EXISTS (
		           SELECT FROM pg_catalog.pg_enum e
		           WHERE e.enumtypid OPERATOR(pg_catalog.=) $1
		             AND e.enumlabel OPERATOR(pg_catalog.=) $2),
		       EXISTS (SELECT FROM own WHERE own.enumtypid OPERATOR(pg_catalog.<>) $1),
		       EXISTS (SELECT FROM own WHERE own.enumtypid OPERATOR(pg_catalog.=) $1),
		       COALESCE(pg_catalog.to_regtype($3)::pg_catalog.oid OPERATOR(pg_catalog.=) $1, false)`
	ref := st.EnumType()
	name := pgx.Identifier{ref.Name}
	if ref.Schema != "" {
		name = pgx.Identifier{ref.Schema, ref.Name}
	}
	var labeled, otherChanged, wrote, stillNamed bool
	if err := tx.QueryRow(ctx, q, typ, st.AddedEnumValue(), name.Sanitize()).Scan(&labeled, &otherChanged, &wrote, &stillNamed); err != nil {
		return fmt.Errorf("confirm the ADD VALUE changed only enum type OID %d: %w", typ, err)
	}
	if otherChanged {
		return fmt.Errorf("%w: another session gave the name to a different type before the statement reached it; nothing was added",
			ErrEnumTypeDrift)
	}
	if !labeled {
		return fmt.Errorf("%w: the checked type does not carry the label after the statement; nothing was added", ErrEnumTypeDrift)
	}
	if !wrote && !stillNamed {
		return fmt.Errorf("%w: the statement wrote nothing, and %s no longer names the checked type, so the type it reached is unproven; nothing was added",
			ErrEnumTypeDrift, name.Sanitize())
	}
	return nil
}
