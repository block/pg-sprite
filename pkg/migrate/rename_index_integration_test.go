package migrate_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/suggest"
	"github.com/block/pg-sprite/pkg/verdict"
)

// renameIndexFixture creates a users table with a plain index on email and
// a unique constraint on handle, whose backing index carries the
// constraint's name.
func renameIndexFixture(t *testing.T) (*pgxpool.Pool, string, string) {
	t.Helper()
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.users (
			id     int PRIMARY KEY,
			email  text NOT NULL,
			handle text NOT NULL,
			CONSTRAINT users_handle_key UNIQUE (handle)
		);
		CREATE INDEX users_email_idx ON %[1]s.users (email)`, schema))
	require.NoError(t, err)
	return pool, schema, url
}

// relationNamed reports whether schema holds a relation (table or index)
// with the given name.
func relationNamed(t *testing.T, pool *pgxpool.Pool, schema, name string) bool {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT count(*) FROM pg_class WHERE relnamespace = $1::regnamespace AND relname = $2`, schema, name).Scan(&n))
	return n == 1
}

// A plain index is named by no application SQL, so its rename runs as a
// metadata-only catalog change. The statement names an index, so the
// verdict names the table that owns it.
func TestRenameIndexRunsAPlainIndexRename(t *testing.T) {
	pool, schema, _ := renameIndexFixture(t)
	sql := fmt.Sprintf(`ALTER INDEX %s.users_email_idx RENAME TO users_contact_email_idx`, schema)

	v, err := migrate.Run(t.Context(), pool, parseOne(t, sql), runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome, v.Detail)
	assert.Equal(t, schema+".users", v.Table, "the verdict names the index's owning table")
	assert.False(t, relationNamed(t, pool, schema, "users_email_idx"))
	assert.True(t, relationNamed(t, pool, schema, "users_contact_email_idx"))
}

// An unqualified index name resolves on the session search_path, as the
// statement does when it runs, and the owning table is found there.
func TestRenameIndexResolvesAnUnqualifiedIndexOnTheSearchPath(t *testing.T) {
	pool, schema, url := renameIndexFixture(t)
	pc, err := pgxpool.ParseConfig(url)
	require.NoError(t, err)
	pc.ConnConfig.RuntimeParams["search_path"] = schema + ", public"
	scoped, err := pgxpool.NewWithConfig(t.Context(), pc)
	require.NoError(t, err)
	t.Cleanup(scoped.Close)

	v, err := migrate.Run(t.Context(), scoped, parseOne(t, `ALTER INDEX users_email_idx RENAME TO users_contact_email_idx`), runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome, v.Detail)
	assert.Equal(t, schema+".users", v.Table)
	assert.True(t, relationNamed(t, pool, schema, "users_contact_email_idx"))
}

// Renaming the index behind a unique constraint renames the constraint,
// which ON CONFLICT ON CONSTRAINT names, so it breaks the application like
// a column rename and is refused by design with the coordination guidance.
// Nothing is renamed.
func TestRenameIndexRefusesAnIndexThatBacksAConstraint(t *testing.T) {
	pool, schema, _ := renameIndexFixture(t)
	sql := fmt.Sprintf(`ALTER INDEX %s.users_handle_key RENAME TO users_username_key`, schema)

	v, err := migrate.Run(t.Context(), pool, parseOne(t, sql), runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonRewriteRequired, v.Reason)
	assert.Equal(t, verdict.ClassByDesign, v.Class)
	assert.Equal(t, suggest.GuidanceCoordinatedConstraintRename, v.Guidance)
	assert.Equal(t, schema+".users", v.Table)

	var constraint string
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT conname FROM pg_constraint WHERE conrelid = $1::regclass AND contype = 'u'`, schema+".users").Scan(&constraint))
	assert.Equal(t, "users_handle_key", constraint, "the constraint must keep its name")
}

// PostgreSQL's ALTER INDEX ... RENAME also renames a table. A name that is
// not an index must not slip a table rename past the app-breaking-rename
// refusal: it is a usage error and nothing executes.
func TestRenameIndexRefusesANameThatIsATable(t *testing.T) {
	pool, schema, _ := renameIndexFixture(t)
	sql := fmt.Sprintf(`ALTER INDEX %s.users RENAME TO accounts`, schema)

	_, err := migrate.Run(t.Context(), pool, parseOne(t, sql), runOptions())
	require.ErrorIs(t, err, migrate.ErrRenamedIndexNotFound)
	assert.True(t, relationNamed(t, pool, schema, "users"), "the table must keep its name")
}

// --force names a table, and an index rename names an index: the
// acknowledgement has nothing to match, so it is a usage error and nothing
// executes.
func TestRenameIndexRejectsTheForceAcknowledgement(t *testing.T) {
	pool, schema, _ := renameIndexFixture(t)
	sql := fmt.Sprintf(`ALTER INDEX %s.users_email_idx RENAME TO users_contact_email_idx`, schema)
	opts := runOptions()
	opts.Force = schema + ".users"

	_, err := migrate.Run(t.Context(), pool, parseOne(t, sql), opts)
	require.ErrorIs(t, err, migrate.ErrIndexRenameAcknowledgement)
	assert.True(t, relationNamed(t, pool, schema, "users_email_idx"), "nothing must have executed")
}
