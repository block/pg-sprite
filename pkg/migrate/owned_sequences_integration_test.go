package migrate_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/verdict"
)

// A column whose default draws on two sequences it owns is something only
// export refuses: a statement on the table reads its facts like any other
// and runs.
func TestRunPlansATableWithAColumnOwningTwoSequences(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	for _, ddl := range []string{
		`CREATE SEQUENCE %[1]s.first_seq`,
		`CREATE SEQUENCE %[1]s.second_seq`,
		`CREATE TABLE %[1]s.t (id bigint PRIMARY KEY DEFAULT nextval('%[1]s.first_seq') + nextval('%[1]s.second_seq'), note text)`,
		`ALTER SEQUENCE %[1]s.first_seq OWNED BY %[1]s.t.id`,
		`ALTER SEQUENCE %[1]s.second_seq OWNED BY %[1]s.t.id`,
	} {
		_, err := pool.Exec(t.Context(), fmt.Sprintf(ddl, schema))
		require.NoError(t, err, ddl)
	}

	v, err := migrate.Run(t.Context(), pool,
		parseOne(t, fmt.Sprintf("CREATE INDEX CONCURRENTLY t_note_idx ON %s.t (note)", schema)), runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	var valid bool
	require.NoError(t, pool.QueryRow(t.Context(),
		"SELECT indisvalid FROM pg_index WHERE indexrelid = $1::regclass", schema+".t_note_idx").Scan(&valid))
	assert.True(t, valid)
}
