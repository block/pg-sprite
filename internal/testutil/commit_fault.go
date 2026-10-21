package testutil

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// LoseConnectionAtCommit makes the next ALTER TABLE whose text contains
// marker lose its backend while the COMMIT is being processed: the DDL queues
// a row in a table guarded by a deferred constraint trigger, and that trigger
// runs inside COMMIT and terminates its own backend. The client's COMMIT then
// ends without a success reply — the same observable state as a COMMIT the
// server applied whose reply never arrived. The probe table, its trigger, and
// the event trigger live in schema and go with it.
func LoseConnectionAtCommit(t *testing.T, pool *pgxpool.Pool, schema, marker string) {
	t.Helper()
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.commit_probe (id integer);
		CREATE FUNCTION %[1]s.lose_connection() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			PERFORM pg_terminate_backend(pg_backend_pid());
			PERFORM pg_sleep(30);
			RETURN NULL;
		END;
		$$;
		CREATE CONSTRAINT TRIGGER lose_connection AFTER INSERT ON %[1]s.commit_probe
			DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION %[1]s.lose_connection()`, schema))
	require.NoError(t, err)
	InstallEventTrigger(t, pool, DDLCommandEnd, schema, "queue_commit_fault", fmt.Sprintf(`
		IF TG_TAG = 'ALTER TABLE' AND current_query() LIKE %s THEN
			INSERT INTO %s.commit_probe VALUES (1);
		END IF;`, quoteLiteral("%"+escapeLikePattern(marker)+"%"), schema))
}
