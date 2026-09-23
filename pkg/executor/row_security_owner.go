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

func checkRowSecurityOwner(ctx context.Context, tx pgx.Tx, schema, table string) error {
	var owner bool
	err := tx.QueryRow(ctx, `SELECT pg_catalog.pg_has_role(current_user, c.relowner, 'USAGE')
 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = $1 AND c.relname = $2`, schema, table).Scan(&owner)
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
	return nil
}
