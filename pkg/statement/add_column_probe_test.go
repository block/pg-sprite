package statement

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Each added column becomes its own probe on the probe table, keeping what
// decides a rewrite and dropping the constraints that only scan or build an
// index, and an identity's SEQUENCE NAME. Commands other than ADD COLUMN,
// and generated columns, are left out, and an IF NOT EXISTS is recorded on
// its probe.
func TestAddColumnProbes(t *testing.T) {
	probes, err := AddColumnProbes(
		"ALTER TABLE app.t ADD COLUMN a timestamptz NOT NULL DEFAULT now(), "+
			"ADD COLUMN b int CONSTRAINT b_pos CHECK (b > 0) UNIQUE REFERENCES app.u (id), "+
			"ADD COLUMN c bigint GENERATED ALWAYS AS IDENTITY (SEQUENCE NAME app.c_seq START WITH 10), "+
			"ADD COLUMN IF NOT EXISTS f text DEFAULT 'x', "+
			"ADD COLUMN e int GENERATED ALWAYS AS (b * 2) STORED, "+
			"ALTER COLUMN d TYPE text",
		"pg_temp", "probe")
	require.NoError(t, err)
	assert.Equal(t, map[string]AddColumnProbe{
		"a": {SQL: "ALTER TABLE pg_temp.probe ADD COLUMN a timestamptz NOT NULL DEFAULT now()"},
		"b": {SQL: "ALTER TABLE pg_temp.probe ADD COLUMN b int"},
		"c": {SQL: "ALTER TABLE pg_temp.probe ADD COLUMN c bigint GENERATED ALWAYS AS IDENTITY (START 10 )"},
		"f": {SQL: "ALTER TABLE pg_temp.probe ADD COLUMN IF NOT EXISTS f text DEFAULT 'x'", IfNotExists: true},
	}, probes)
}

// A column added twice in one statement has no single answer, generated or
// not, so the statement is refused rather than proved by whichever probe
// was built last.
func TestAddColumnProbesRefusesARepeatedColumn(t *testing.T) {
	for _, sql := range []string{
		"ALTER TABLE t ADD COLUMN a int, ADD COLUMN a int DEFAULT 1",
		"ALTER TABLE t ADD COLUMN a int GENERATED ALWAYS AS (1) STORED, ADD COLUMN IF NOT EXISTS a int",
	} {
		_, err := AddColumnProbes(sql, "pg_temp", "probe")
		require.ErrorIs(t, err, ErrRepeatedAddColumn, sql)
		assert.Contains(t, err.Error(), `column "a"`, sql)
	}
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
