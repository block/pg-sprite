package checkpoint

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/progress"
)

// Zero options take the defaults; a store cannot be built over no pool or
// under options that would never read.
func TestNewStoreOptions(t *testing.T) {
	_, err := NewStore(nil, Options{})
	assert.ErrorIs(t, err, ErrInvalidOptions)

	opts := Options{}.withDefaults()
	assert.Equal(t, progress.WallClock{}, opts.Clock)
	assert.Equal(t, DefaultLoadAttempts, opts.LoadAttempts)
	assert.Equal(t, DefaultLoadBackoff, opts.LoadBackoff)
	assert.NotNil(t, opts.Sleep)
	require.NoError(t, opts.validate())

	assert.ErrorIs(t, Options{LoadAttempts: -1}.withDefaults().validate(), ErrInvalidOptions)
	assert.ErrorIs(t, Options{LoadBackoff: -time.Second}.withDefaults().validate(), ErrInvalidOptions)
}

// The table identifier is spelled as a constant so the statements can be
// constants too; it must stay what pgx would render for the two names.
func TestTableIdentMatchesPgxQuoting(t *testing.T) {
	assert.Equal(t, pgx.Identifier{SchemaName, TableName}.Sanitize(), tableIdent)
	assert.Equal(t, "CREATE SCHEMA "+pgx.Identifier{SchemaName}.Sanitize(), createSchemaSQL)
	assert.True(t, strings.HasPrefix(createTableSQL(), "CREATE TABLE "+tableIdent+" ("), createTableSQL())
}

// The column list is the one statement of the table's shape: the CREATE
// TABLE names every column in it with its definition and nothing else, the
// ADD COLUMN for a column spells it the same way, and the primary key is
// the one-row-per-target key (ST-1).
func TestCreateTableSQLRendersEveryColumn(t *testing.T) {
	sql := createTableSQL()
	assert.Equal(t, 1, strings.Count(sql, "PRIMARY KEY (schema_name, table_name)"))
	body := strings.TrimSuffix(strings.TrimPrefix(sql, "CREATE TABLE "+tableIdent+" (\n"), "\n)")
	lines := strings.Split(body, "\n")
	columns := tableColumns()
	require.Len(t, lines, len(columns)+1, "one line per column plus the constraint")
	for i, c := range columns {
		assert.Equal(t, "\t"+c.name+" "+c.definition+",", lines[i])
		assert.Equal(t, "ALTER TABLE "+tableIdent+" ADD COLUMN "+c.name+" "+c.definition, addColumnSQL(c))
	}
	assert.Equal(t, "\t"+tableConstraintSQL, lines[len(columns)])
}

// Every column after the first shipped shape is nullable: Ensure adds it to
// a table that may already hold rows, and ADD COLUMN … NOT NULL without a
// default fails on one. The first shape is spelled out here so a column
// quietly added to it, where Ensure would never add it, is caught too.
func TestColumnsAddedToAnExistingTableAreNullable(t *testing.T) {
	firstShape := []string{"schema_name", "table_name", "format_version", "shadow_table", "slot_name", "publication_name",
		"watermark", "last_applied_lsn", "source_fingerprint", "target_fingerprint", "phase", "updated_at"}
	columns := tableColumns()
	require.GreaterOrEqual(t, len(columns), len(firstShape))
	for i, name := range firstShape {
		assert.Equal(t, name, columns[i].name, "the first shape is fixed")
	}
	for _, c := range columns[len(firstShape):] {
		assert.NotContains(t, c.definition, "NOT NULL", "%s is added to tables that hold rows", c.name)
	}
}

// The load statement reads back every column Save writes, so a column
// added to the shape cannot be written and forgotten.
func TestSaveAndLoadSQLNameEveryColumn(t *testing.T) {
	inserted, _, found := strings.Cut(saveSQL, ") VALUES (")
	require.True(t, found)
	selected, _, found := strings.Cut(loadSQL, "\nFROM ")
	require.True(t, found)
	for _, c := range tableColumns() {
		assert.Contains(t, inserted, c.name, "save writes %s", c.name)
		if c.name == "schema_name" || c.name == "table_name" {
			continue
		}
		assert.Contains(t, selected, c.name, "load reads %s", c.name)
	}
}

// Delete's statement matches the whole row identity, so a row that another
// statement wrote after the caller read its IncompatibleError is never the
// one removed (ST-2).
func TestDeleteSQLMatchesTheRowIdentity(t *testing.T) {
	_, where, found := strings.Cut(deleteSQL, " WHERE ")
	require.True(t, found, "the delete is conditional")
	for _, column := range []string{"schema_name", "table_name", "format_version", "source_fingerprint", "target_fingerprint"} {
		assert.Contains(t, where, column+" = $")
	}
}

// The copier's zero watermark is the NULL column, and a valid one carries
// its key; the complete watermark fits the bigint column.
func TestWatermarkColumn(t *testing.T) {
	assert.Equal(t, pgtype.Int8{}, watermarkColumn(copier.Watermark{}))
	assert.Equal(t, pgtype.Int8{Int64: 1000, Valid: true}, watermarkColumn(copier.NewWatermark(1000)))
	assert.Equal(t, pgtype.Int8{Int64: 1<<63 - 1, Valid: true}, watermarkColumn(copier.NewWatermark(1<<63-1)))
}

// Save's one statement upserts on the target key and updates only a row
// that carries this run's identity: every identity column is in the
// conflict guard and none is in the update list, so a Save can never move
// a row from one statement to another (ST-1, ST-2).
func TestSaveSQLGuardsTheRowIdentity(t *testing.T) {
	assert.Contains(t, saveSQL, `ON CONFLICT (schema_name, table_name) DO UPDATE SET`)
	_, guard, found := strings.Cut(saveSQL, "\nWHERE ")
	require.True(t, found, "the upsert has a conflict guard")
	for _, column := range []string{"format_version", "source_fingerprint", "target_fingerprint"} {
		assert.Contains(t, guard, `"pgsprite"."pgsprite_checkpoint".`+column+" = EXCLUDED."+column)
	}
	assert.Contains(t, guard, `"pgsprite"."pgsprite_checkpoint".phase = ANY($15)`, "the stored row's phase must be one this engine knows")
	update, _, _ := strings.Cut(saveSQL, "\nWHERE ")
	_, update, _ = strings.Cut(update, "DO UPDATE SET")
	for _, column := range []string{"format_version", "source_fingerprint", "target_fingerprint"} {
		assert.NotContains(t, update, column+" = EXCLUDED."+column, "identity columns are never updated")
	}
}

// Save's statement keeps a proof once written: the update falls back to
// the row's proof when the Save carries none, and the guard admits a Save
// only when one side has no proof or both hold the same one, so no later
// Save clears or replaces it (ST-2).
func TestSaveSQLKeepsTheShadowProofOnceWritten(t *testing.T) {
	update, guard, found := strings.Cut(saveSQL, "\nWHERE ")
	require.True(t, found, "the upsert has a conflict guard")
	assert.Contains(t, update, `shadow_proof = COALESCE(EXCLUDED.shadow_proof, "pgsprite"."pgsprite_checkpoint".shadow_proof)`)
	assert.NotContains(t, update, "shadow_proof = EXCLUDED.shadow_proof", "a missing proof never overwrites the row's")
	assert.Contains(t, guard, "EXCLUDED.shadow_proof IS NULL")
	assert.Contains(t, guard, `"pgsprite"."pgsprite_checkpoint".shadow_proof IS NULL`)
	assert.Contains(t, guard, `"pgsprite"."pgsprite_checkpoint".shadow_proof = EXCLUDED.shadow_proof`)
}
