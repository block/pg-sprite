package schemachange_test

import (
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// A checkpoint stores the built shadow as JSON and a resume decodes it back
// into a Proof to compare with what InspectShadow found. The round trip has
// to be lossless through every nested field, so the table carries an
// identity column, storage parameters, a grant and a policy: a missing tag
// on any nested type would surface as an inequality here.
func TestBuiltShadowJSONRoundTripsAsTheProofInspectionRederives(t *testing.T) {
	cfg := dbconn.Config{URL: testutil.StartPostgres(t)}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	reader := testutil.NewRole(t, pool, "NOLOGIN")
	f := shadowFixture{cfg: cfg, pool: pool, schema: testutil.NewSchema(t, pool)}
	f.exec(t, `
		CREATE TABLE %s.orders (
			id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			tenant text NOT NULL,
			qty integer NOT NULL
		) WITH (fillfactor = 70)`)
	f.exec(t, `COMMENT ON TABLE %s.orders IS 'customer orders'`)
	f.exec(t, `GRANT SELECT ON %s.orders TO `+pgx.Identifier{reader}.Sanitize())
	f.exec(t, `ALTER TABLE %s.orders ENABLE ROW LEVEL SECURITY`)
	f.exec(t, `CREATE POLICY tenant_read ON %s.orders FOR SELECT TO `+pgx.Identifier{reader}.Sanitize()+` USING (tenant = current_user)`)

	lock := f.lock(t, "orders")
	target := f.prove(t, "orders")
	built, err := schemachange.BuildShadow(t.Context(), f.pool, lock, target, f.alter(t, `ALTER TABLE %s.orders ALTER COLUMN qty TYPE bigint`), schemachange.Options{})
	require.NoError(t, err)

	proof := built.Proof()
	assert.Equal(t, built.Schema(), proof.Schema)
	assert.Equal(t, built.SourceTable(), proof.SourceTable)
	assert.Equal(t, built.ShadowTable(), proof.ShadowTable)
	assert.Equal(t, built.SourceOID(), proof.SourceOID)
	assert.Equal(t, built.ShadowOID(), proof.ShadowOID)
	assert.Equal(t, built.SourceFingerprint(), proof.SourceFingerprint)
	assert.Equal(t, built.TargetFingerprint(), proof.TargetFingerprint)
	assert.Equal(t, built.IdentityColumns(), proof.IdentityColumns)
	assert.Equal(t, built.Fidelity(), proof.Fidelity)
	assert.Equal(t, built.CopyColumns(), proof.CopyColumns)
	require.NotEmpty(t, proof.IdentityColumns, "the fixture exercises the identity handoff fields")
	require.NotEmpty(t, proof.Fidelity.Grants, "the fixture exercises the grant fields")
	require.NotEmpty(t, proof.Fidelity.Policies, "the fixture exercises the policy fields")
	require.NotEmpty(t, proof.Fidelity.RelOptions, "the fixture exercises the storage parameters")

	encoded, err := json.Marshal(built)
	require.NoError(t, err)
	var decoded schemachange.Proof
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	assert.Equal(t, proof, decoded, "the checkpoint decodes to the proof the build returned")

	inspected, err := schemachange.InspectShadow(t.Context(), f.pool, lock, target, schemachange.Options{})
	require.NoError(t, err)
	assert.Equal(t, inspected.Proof(), decoded, "the resume comparison: inspection re-derives the checkpointed proof")

	var keys map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(encoded, &keys))
	assert.ElementsMatch(t, []string{
		"schema", "source_table", "shadow_table", "source_oid", "shadow_oid",
		"source_fingerprint", "target_fingerprint", "identity_columns", "fidelity", "copy_columns",
	}, mapKeys(keys), "the checkpoint's wire shape is the Proof's field set")
}

func mapKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
