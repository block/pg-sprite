package migrate

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/statement"
)

// RunDesiredRowSecurity converges a desired file that declares row security.
// When the table exists, executor.ExecuteRowSecurity converges its row
// security. When the name is free, the absence and creation-access proofs
// are minted here, in the session that executes, and
// executor.ExecuteCreateWithRowSecurity creates the table and its row
// security in one transaction, so the table is never visible without its
// policies. A standalone type holding the name refuses as a create
// collision.
//
// Only opts.CreateOwner, opts.Budget.Brief, and opts.Logger are read. The
// report lists every committed statement in execution order; an error
// returns a zero report, and migrate.RowSecurityRefusal says whether it is
// a refusal.
func RunDesiredRowSecurity(ctx context.Context, pool *pgxpool.Pool, schema string, desired statement.DesiredWithRowSecurity, opts Options) (executor.RowSecurityReport, error) {
	at, role, err := createWithRowSecurityProofs(ctx, pool, schema, desired, opts)
	if errors.Is(err, preflight.ErrRelationExists) {
		return executor.ExecuteRowSecurity(ctx, pool, schema, desired, opts.Budget.Brief)
	}
	if err != nil {
		return executor.RowSecurityReport{}, err
	}
	return executor.ExecuteCreateWithRowSecurity(ctx, pool, at, role, desired, opts.Budget.Brief)
}

// PreviewCreateWithRowSecurity mints the same proofs as
// RunDesiredRowSecurity for an absent table and returns the statements its
// create would run, from a transaction that rolls back. A table already at
// the name returns an error wrapping preflight.ErrRelationExists, and the
// caller previews the existing table instead.
func PreviewCreateWithRowSecurity(ctx context.Context, pool *pgxpool.Pool, schema string, desired statement.DesiredWithRowSecurity, opts Options) (executor.RowSecurityPlan, error) {
	at, role, err := createWithRowSecurityProofs(ctx, pool, schema, desired, opts)
	if err != nil {
		return executor.RowSecurityPlan{}, err
	}
	return executor.PreviewCreateWithRowSecurity(ctx, pool, at, role, desired, opts.Budget.Brief)
}

// createWithRowSecurityProofs mints the absence and creation-access proofs
// for the desired table. A table at the name returns an error wrapping
// preflight.ErrRelationExists; a standalone type there is a create
// collision.
func createWithRowSecurityProofs(ctx context.Context, pool *pgxpool.Pool, schema string, desired statement.DesiredWithRowSecurity, opts Options) (preflight.AbsentTarget, preflight.CreationRole, error) {
	at, err := preflight.CheckTableAbsent(ctx, pool, schema, desired.Table())
	if errors.Is(err, preflight.ErrRelationExists) {
		return preflight.AbsentTarget{}, preflight.CreationRole{}, err
	}
	if errors.Is(err, preflight.ErrTypeExists) {
		return preflight.AbsentTarget{}, preflight.CreationRole{}, fmt.Errorf("%w: %w", executor.ErrCreateCollision, err)
	}
	if err != nil {
		return preflight.AbsentTarget{}, preflight.CreationRole{}, fmt.Errorf("check whether %s.%s exists: %w", schema, desired.Table(), err)
	}
	role, err := preflight.CheckCreatePrivilegesAs(ctx, pool, at.Schema(), opts.CreateOwner)
	if err != nil {
		return preflight.AbsentTarget{}, preflight.CreationRole{}, fmt.Errorf("verify creation access in schema %s: %w", at.Schema(), err)
	}
	opts.logger().Debug("create with row security preflight passed",
		"schema", at.Schema(), "table", at.Table(), "role", role.Role(), "create_owner", role.Owner())
	return at, role, nil
}
