package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrRowSecurityRefused means the declaration, target shape, or privileges must
// change before retrying. Wrapped causes retain the specific admission failure.
var ErrRowSecurityRefused = errors.New("row security change refused")

func checkRowSecurityPrivileges(ctx context.Context, tx pgx.Tx, schema, table string) error {
	var owner, createSchema bool
	err := tx.QueryRow(ctx, `SELECT pg_catalog.pg_has_role(current_user, c.relowner, 'USAGE'),
 pg_catalog.has_database_privilege(current_user, pg_catalog.current_database(), 'CREATE')
 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = $1 AND c.relname = $2`, schema, table).Scan(&owner, &createSchema)
	target := pgx.Identifier{schema, table}.Sanitize()
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("check row security owner for %s: %w", target, ErrTableNotFound)
	}
	if err != nil {
		return fmt.Errorf("check row security owner for %s: %w", target, err)
	}
	if !owner {
		return fmt.Errorf("row security on %s requires owner privileges: %w", target, ErrRowSecurityRefused)
	}
	if !createSchema {
		return fmt.Errorf("row security on %s requires CREATE on the database for scratch inspection: %w", target, ErrRowSecurityRefused)
	}
	return nil
}
