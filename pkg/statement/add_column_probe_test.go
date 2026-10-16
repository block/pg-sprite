package statement

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Each added column becomes its own probe on the probe table, keeping what
// decides a rewrite and dropping the constraints that only scan or build an
// index. Commands other than ADD COLUMN, and generated columns, are left
// out.
func TestAddColumnProbes(t *testing.T) {
	probes, err := AddColumnProbes(
		"ALTER TABLE app.t ADD COLUMN a timestamptz NOT NULL DEFAULT now(), "+
			"ADD COLUMN b int CONSTRAINT b_pos CHECK (b > 0) UNIQUE REFERENCES app.u (id), "+
			"ADD COLUMN c bigint GENERATED ALWAYS AS IDENTITY, "+
			"ADD COLUMN e int GENERATED ALWAYS AS (b * 2) STORED, "+
			"ALTER COLUMN d TYPE text",
		"pg_temp", "probe")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"a": "ALTER TABLE pg_temp.probe ADD COLUMN a timestamptz NOT NULL DEFAULT now()",
		"b": "ALTER TABLE pg_temp.probe ADD COLUMN b int",
		"c": "ALTER TABLE pg_temp.probe ADD COLUMN c bigint GENERATED ALWAYS AS IDENTITY",
	}, probes)
}

func TestAddColumnProbesWithoutAnAddedColumn(t *testing.T) {
	probes, err := AddColumnProbes("ALTER TABLE t DROP COLUMN a", "pg_temp", "probe")
	require.NoError(t, err)
	assert.Empty(t, probes)
}

func TestAddColumnProbesRefusesOtherStatements(t *testing.T) {
	_, err := AddColumnProbes("CREATE INDEX i ON t (a)", "pg_temp", "probe")
	require.ErrorIs(t, err, ErrNotAlterTable)
}
