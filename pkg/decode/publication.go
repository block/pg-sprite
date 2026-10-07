package decode

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/preflight"
)

// ensurePublication creates the publication of the derived name publishing
// exactly the target table, or verifies that the one already there does. The
// pool is proven to be on the target's database first, so the publication
// cannot land in a database the slot is not named for.
func ensurePublication(ctx context.Context, pool *pgxpool.Pool, name string, target preflight.CopySwapTarget) error {
	var onTargetDatabase bool
	err := pool.QueryRow(ctx, `SELECT pg_catalog.current_database() = $1`, target.Database()).Scan(&onTargetDatabase)
	if err != nil {
		return fmt.Errorf("create publication %s: %w", name, err)
	}
	if !onTargetDatabase {
		return fmt.Errorf("%w: ST-3: pool is not on the target's database %q", ErrInvariantViolation, target.Database())
	}

	table := pgx.Identifier{target.Schema(), target.Table()}.Sanitize()
	_, err = pool.Exec(ctx, `CREATE PUBLICATION `+pgx.Identifier{name}.Sanitize()+` FOR TABLE `+table)
	switch {
	case err == nil:
		return nil
	case isSQLState(err, sqlstateDuplicateObject):
		return verifyPublication(ctx, pool, name, target)
	case isSQLState(err, sqlstateInsufficientPrivs):
		return fmt.Errorf("create publication %s for %s: %w (the role needs CREATE on database %s)",
			name, table, err, pgx.Identifier{target.Database()}.Sanitize())
	default:
		return fmt.Errorf("create publication %s for %s: %w", name, table, err)
	}
}

// verifyPublication admits an existing publication of the derived name only
// when it publishes the target table and nothing else; anything wider is
// someone else's and is refused rather than adopted.
func verifyPublication(ctx context.Context, pool *pgxpool.Pool, name string, target preflight.CopySwapTarget) error {
	var allTables bool
	var publishesTarget, published int64
	err := pool.QueryRow(ctx, `
		SELECT p.puballtables,
		       (SELECT pg_catalog.count(*) FROM pg_catalog.pg_publication_tables t
		         WHERE t.pubname = p.pubname AND t.schemaname = $2 AND t.tablename = $3),
		       (SELECT pg_catalog.count(*) FROM pg_catalog.pg_publication_tables t
		         WHERE t.pubname = p.pubname)
		FROM pg_catalog.pg_publication p
		WHERE p.pubname = $1`, name, target.Schema(), target.Table()).
		Scan(&allTables, &publishesTarget, &published)
	if err != nil {
		return fmt.Errorf("verify publication %s: %w", name, err)
	}
	if allTables {
		return fmt.Errorf("%w: publication %s is FOR ALL TABLES", ErrForeignDecodingState, name)
	}
	if publishesTarget != 1 || published != 1 {
		return fmt.Errorf("%w: publication %s publishes %d tables, %d of them the target",
			ErrForeignDecodingState, name, published, publishesTarget)
	}
	return nil
}
