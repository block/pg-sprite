package checkpoint_test

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/copier"
)

// firstShapeTableSQL is the checkpoint table as the first engine created
// it: every column of today's shape up to updated_at, and no shadow_proof.
// It is spelled out here because it is what a database that engine ran
// against still holds, and Ensure must take it from here to today's shape.
const firstShapeTableSQL = `CREATE TABLE "pgsprite"."pgsprite_checkpoint" (
	schema_name        text NOT NULL,
	table_name         text NOT NULL,
	format_version     integer NOT NULL,
	shadow_table       text NOT NULL,
	slot_name          text NOT NULL,
	publication_name   text NOT NULL,
	watermark          bigint,
	last_applied_lsn   pg_lsn NOT NULL,
	source_fingerprint text NOT NULL,
	target_fingerprint text NOT NULL,
	phase              text NOT NULL,
	updated_at         timestamptz NOT NULL,
	PRIMARY KEY (schema_name, table_name)
)`

// firstShapeRowSQL is a row that first engine left behind, in its format.
const firstShapeRowSQL = `INSERT INTO "pgsprite"."pgsprite_checkpoint" (
	schema_name, table_name, format_version, shadow_table, slot_name, publication_name,
	watermark, last_applied_lsn, source_fingerprint, target_fingerprint, phase, updated_at
) VALUES ('app', 'orders', 1, '_pgsprite_orders_new', '', '', 1000, '16/B374D848', 'src-a', 'tgt-a', 'copying', now())`

// columnNames lists the checkpoint table's live columns in order.
func (f storeFixture) columnNames(t *testing.T) []string {
	t.Helper()
	rows, err := f.pool.Query(t.Context(), `
SELECT a.attname
FROM pg_attribute a
JOIN pg_class c ON c.oid = a.attrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relname = $2 AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY a.attnum`, checkpoint.SchemaName, checkpoint.TableName)
	require.NoError(t, err)
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	return names
}

// A table the first engine created lacks shadow_proof. Ensure adds it in
// place, so the table keeps the row that engine wrote — which Inspect then
// reports as a row in another format, with its identity, for the explicit
// fresh start — and a Save in today's format lands with its proof.
func TestEnsureAddsTheColumnsAnOlderTableLacks(t *testing.T) {
	f := newStoreFixture(t)
	f.exec(t, `CREATE SCHEMA "pgsprite"`)
	f.exec(t, firstShapeTableSQL)
	f.exec(t, firstShapeRowSQL)
	before := f.columnNames(t)
	require.NotContains(t, before, "shadow_proof", "the first shape has no proof column")

	require.NoError(t, f.store.Ensure(t.Context()))
	assert.Equal(t, append(before, "shadow_proof"), f.columnNames(t), "the missing column is added last; nothing else moves")
	assert.Equal(t, int64(1), f.rowCount(t), "the older engine's row survives the evolution")
	require.NoError(t, f.store.Ensure(t.Context()), "a second Ensure finds nothing to add")

	_, err := f.store.Inspect(t.Context(), "app", "orders")
	var incompatible *checkpoint.IncompatibleError
	require.ErrorAs(t, err, &incompatible)
	assert.Equal(t, checkpoint.MismatchFormat, incompatible.Mismatch)
	assert.Equal(t, "1", incompatible.Have)
	assert.Equal(t, checkpoint.Identity{FormatVersion: 1, Fingerprints: checkpoint.Fingerprints{Source: "src-a", Target: "tgt-a"}}, incompatible.Stored)

	lock := f.ordersLock(t)
	require.NoError(t, f.store.Delete(t.Context(), lock, "app", "orders", incompatible.Stored))
	cp := ordersCheckpoint()
	cp.ShadowProof = ordersProof
	require.NoError(t, f.store.Save(t.Context(), lock, cp))
	got, err := f.store.Load(t.Context(), "app", "orders", cp.Fingerprints())
	require.NoError(t, err)
	assert.JSONEq(t, string(ordersProof), string(got.ShadowProof))
	assert.Equal(t, copier.NewWatermark(1000), got.Watermark)
}

// A table with a column this engine does not know was shaped by a newer
// engine. Ensure refuses it, typed, and runs no DDL — not even to add a
// column the table also lacks — so the newer engine finds its table as it
// left it.
func TestEnsureRefusesATableANewerEngineShaped(t *testing.T) {
	f := newStoreFixture(t)
	f.exec(t, `CREATE SCHEMA "pgsprite"`)
	f.exec(t, firstShapeTableSQL)
	f.exec(t, `ALTER TABLE "pgsprite"."pgsprite_checkpoint" ADD COLUMN swap_proof jsonb`)
	before := f.columnNames(t)

	err := f.store.Ensure(t.Context())
	assert.ErrorIs(t, err, checkpoint.ErrTableNewer)
	assert.ErrorContains(t, err, "swap_proof")
	assert.Equal(t, before, f.columnNames(t), "a refused table is left exactly as found: shadow_proof is not added")
}

// A table of today's shape with one column more is refused the same way,
// even though nothing is missing: the extra column is what marks it as a
// newer engine's.
func TestEnsureRefusesACurrentTableWithAnUnknownColumn(t *testing.T) {
	f := newStoreFixture(t)
	f.ensure(t)
	f.exec(t, `ALTER TABLE "pgsprite"."pgsprite_checkpoint" ADD COLUMN operator_note text`)

	err := f.store.Ensure(t.Context())
	assert.ErrorIs(t, err, checkpoint.ErrTableNewer)
	assert.ErrorContains(t, err, "operator_note")
}
