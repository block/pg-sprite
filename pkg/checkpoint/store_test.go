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
	assert.True(t, strings.HasPrefix(createTableSQL, "CREATE TABLE "+tableIdent+" ("), createTableSQL)
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
	update, _, _ := strings.Cut(saveSQL, "\nWHERE ")
	_, update, _ = strings.Cut(update, "DO UPDATE SET")
	for _, column := range []string{"format_version", "source_fingerprint", "target_fingerprint"} {
		assert.NotContains(t, update, column+" = EXCLUDED."+column, "identity columns are never updated")
	}
}
