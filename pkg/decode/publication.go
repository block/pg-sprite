package decode

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/preflight"
)

// PublicationPrivilegeError is CreateSlot's refusal when the connected role
// may not create the route's publication. CREATE PUBLICATION needs the
// named privilege on the database; nothing has been created when it is
// returned. It wraps the server's own error, so the SQLSTATE stays
// reachable through errors.As.
type PublicationPrivilegeError struct {
	Publication string
	Database    string
	// Privilege is the database privilege the role lacks.
	Privilege string
	Err       error
}

func (e *PublicationPrivilegeError) Error() string {
	return fmt.Sprintf("create publication %s: %v (the role needs %s on database %s)",
		e.Publication, e.Err, e.Privilege, pgx.Identifier{e.Database}.Sanitize())
}

// Unwrap exposes the server's error.
func (e *PublicationPrivilegeError) Unwrap() error { return e.Err }

// privilegeCreateOnDatabase is what CREATE PUBLICATION requires.
const privilegeCreateOnDatabase = "CREATE"

// ensurePublication creates the publication of the derived name publishing
// exactly the target table, or verifies that the one already there is the
// one this function would have created. The pool is proven to be on the
// target's database first, so the publication cannot land in a database
// the slot is not named for.
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
		return &PublicationPrivilegeError{Publication: name, Database: target.Database(),
			Privilege: privilegeCreateOnDatabase, Err: err}
	default:
		return fmt.Errorf("create publication %s for %s: %w", name, table, err)
	}
}

// verifyPublication admits an existing publication of the derived name only
// when it is exactly what ensurePublication creates: every operation on the
// target table and nothing else. A publication that publishes less — one
// operation, a row filter, a column list — would leave changes out of the
// stream that the swap then loses; one that publishes more is someone
// else's. Either is refused rather than adopted.
func verifyPublication(ctx context.Context, pool *pgxpool.Pool, name string, target preflight.CopySwapTarget) error {
	shape, found, err := readPublicationShape(ctx, pool, name)
	if err != nil {
		return fmt.Errorf("verify publication %s: %w", name, err)
	}
	if !found {
		return fmt.Errorf("verify publication %s: the publication that refused the create is gone", name)
	}
	if detail := shape.notTheRoutes(); detail != "" {
		return foreignPublication(name, detail)
	}
	published := shape.tables[0]
	if published.schema != target.Schema() || published.table != target.Table() {
		return foreignPublication(name, fmt.Sprintf("it publishes %s, not the target %s",
			pgx.Identifier{published.schema, published.table}.Sanitize(),
			pgx.Identifier{target.Schema(), target.Table()}.Sanitize()))
	}
	return nil
}

// dropOwnPublication drops the publication of the derived name when it is
// one this package created, and refuses with a ForeignStateError when it is
// not. The one fact the drop cannot check without the target is which table
// the publication publishes. A publication already absent is success.
func dropOwnPublication(ctx context.Context, pool *pgxpool.Pool, name string) error {
	shape, found, err := readPublicationShape(ctx, pool, name)
	if err != nil {
		return fmt.Errorf("drop publication %s: %w", name, err)
	}
	if !found {
		return nil
	}
	if detail := shape.notTheRoutes(); detail != "" {
		return foreignPublication(name, detail)
	}
	if _, err := pool.Exec(ctx, `DROP PUBLICATION IF EXISTS `+pgx.Identifier{name}.Sanitize()); err != nil {
		return fmt.Errorf("drop publication %s: %w", name, err)
	}
	return nil
}

// publicationShape is what the catalog says a publication publishes, read
// the same way for the create path and the drop path so a publication one
// refuses as foreign the other never drops.
type publicationShape struct {
	allTables                        bool
	insert, update, delete, truncate bool
	viaRoot                          bool
	// schemas counts FOR TABLES IN SCHEMA memberships; a server that cannot
	// express them reports zero.
	schemas int64
	tables  []publishedTable
}

// publishedTable is one explicit table membership of a publication.
type publishedTable struct {
	schema, table string
	// rowFilter and columnList report a WHERE clause and a column list on
	// the membership; a server that cannot express them reports false.
	rowFilter, columnList bool
}

// publicationFiltersSince is the server_version_num from which a
// publication can carry a row filter, a column list, or a whole schema.
const publicationFiltersSince = 150000

// readPublicationShape reads the named publication's shape; found is false
// when there is no publication of the name.
func readPublicationShape(ctx context.Context, pool *pgxpool.Pool, name string) (shape publicationShape, found bool, err error) {
	var version int
	err = pool.QueryRow(ctx, `
		SELECT p.puballtables, p.pubinsert, p.pubupdate, p.pubdelete, p.pubtruncate, p.pubviaroot,
		       pg_catalog.current_setting('server_version_num')::int
		FROM pg_catalog.pg_publication p
		WHERE p.pubname = $1`, name).
		Scan(&shape.allTables, &shape.insert, &shape.update, &shape.delete, &shape.truncate, &shape.viaRoot, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return publicationShape{}, false, nil
	}
	if err != nil {
		return publicationShape{}, false, fmt.Errorf("read publication: %w", err)
	}
	filters := version >= publicationFiltersSince
	if shape.tables, err = readPublishedTables(ctx, pool, name, filters); err != nil {
		return publicationShape{}, false, err
	}
	if filters {
		if shape.schemas, err = readPublishedSchemas(ctx, pool, name); err != nil {
			return publicationShape{}, false, err
		}
	}
	return shape, true, nil
}

// readPublishedTables lists the publication's explicit table memberships.
// The filter columns exist only on a server that can express filters, so
// the older server's query reports none.
func readPublishedTables(ctx context.Context, pool *pgxpool.Pool, name string, filters bool) ([]publishedTable, error) {
	filterColumns := `false, false`
	if filters {
		filterColumns = `r.prqual IS NOT NULL, r.prattrs IS NOT NULL`
	}
	rows, err := pool.Query(ctx, `
		SELECT n.nspname, c.relname, `+filterColumns+`
		FROM pg_catalog.pg_publication_rel r
		JOIN pg_catalog.pg_publication p ON p.oid = r.prpubid
		JOIN pg_catalog.pg_class c ON c.oid = r.prrelid
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE p.pubname = $1
		ORDER BY n.nspname, c.relname`, name)
	if err != nil {
		return nil, fmt.Errorf("read published tables: %w", err)
	}
	tables, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (publishedTable, error) {
		var t publishedTable
		err := row.Scan(&t.schema, &t.table, &t.rowFilter, &t.columnList)
		return t, err
	})
	if err != nil {
		return nil, fmt.Errorf("read published tables: %w", err)
	}
	return tables, nil
}

// readPublishedSchemas counts the publication's FOR TABLES IN SCHEMA
// memberships.
func readPublishedSchemas(ctx context.Context, pool *pgxpool.Pool, name string) (int64, error) {
	var schemas int64
	err := pool.QueryRow(ctx, `
		SELECT pg_catalog.count(*)
		FROM pg_catalog.pg_publication_namespace pn
		JOIN pg_catalog.pg_publication p ON p.oid = pn.pnpubid
		WHERE p.pubname = $1`, name).Scan(&schemas)
	if err != nil {
		return 0, fmt.Errorf("read published schemas: %w", err)
	}
	return schemas, nil
}

// publishesEveryOperation reports whether every change to a published
// table reaches the stream.
func (s publicationShape) publishesEveryOperation() bool {
	return s.insert && s.update && s.delete && s.truncate
}

// notTheRoutes says, in words, why the publication is not one
// ensurePublication would have created — apart from which table it
// publishes, which only the create path knows. It is empty when the shape
// is the route's: one plain table membership, every operation, nothing
// else.
func (s publicationShape) notTheRoutes() string {
	switch {
	case s.allTables:
		return "it is FOR ALL TABLES"
	case s.schemas != 0:
		return "it publishes whole schemas"
	case !s.publishesEveryOperation():
		return "it does not publish every operation (insert, update, delete, truncate)"
	case s.viaRoot:
		return "it publishes through the partition root"
	case len(s.tables) != 1:
		return fmt.Sprintf("it publishes %d tables", len(s.tables))
	case s.tables[0].rowFilter:
		return "it publishes the table through a row filter"
	case s.tables[0].columnList:
		return "it publishes a column list of the table"
	default:
		return ""
	}
}
