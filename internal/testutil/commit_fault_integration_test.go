package testutil

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// The lost-commit fault is a database-wide event trigger, and under a shared
// PG_DSN database every package's DDL runs past it. Another schema's ALTER
// TABLE whose text carries the same marker must keep its connection: the
// fault is armed only for the faulted schema's own objects.
func TestLoseConnectionAtCommitIgnoresAnotherSchemasAlter(t *testing.T) {
	pool := privatePool(t)
	faulted := NewSchema(t, pool)
	elsewhere := NewSchema(t, pool)
	LoseConnectionAtCommit(t, pool, faulted, "ADD COLUMN lost_commit")

	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.t (
			id int PRIMARY KEY
		);
		ALTER TABLE %[1]s.t ADD COLUMN lost_commit int`, elsewhere))

	require.NoError(t, err, "another schema's ALTER TABLE keeps its connection")
}
