package schemachange_test

import (
	"encoding/json"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/schemachange"
)

// proofKeyPaths is the wire shape of an encoded Proof: every key at every
// depth, with `[]` marking descent into an array element. A checkpoint
// written by one build is decoded by the next, so a renamed or untagged
// field anywhere in the tree is a compatibility break this list pins.
var proofKeyPaths = []string{
	"schema",
	"source_table",
	"shadow_table",
	"source_oid",
	"shadow_oid",
	"source_fingerprint",
	"target_fingerprint",
	"identity_columns",
	"identity_columns[].column",
	"identity_columns[].always",
	"identity_columns[].sequence_schema",
	"identity_columns[].sequence_name",
	"identity_columns[].options",
	"identity_columns[].options.start",
	"identity_columns[].options.increment",
	"identity_columns[].options.min",
	"identity_columns[].options.max",
	"identity_columns[].options.cache",
	"identity_columns[].options.cycle",
	"fidelity",
	"fidelity.owner",
	"fidelity.replica_identity",
	"fidelity.rls_enabled",
	"fidelity.rls_forced",
	"fidelity.comment",
	"fidelity.tablespace",
	"fidelity.rel_options",
	"fidelity.grants",
	"fidelity.grants[].privilege",
	"fidelity.grants[].grantee",
	"fidelity.grants[].public",
	"fidelity.grants[].grantable",
	"fidelity.column_grants",
	"fidelity.column_grants[].column",
	"fidelity.column_grants[].privilege",
	"fidelity.column_grants[].grantee",
	"fidelity.column_grants[].public",
	"fidelity.column_grants[].grantable",
	"fidelity.policies",
	"fidelity.policies[].name",
	"fidelity.policies[].permissive",
	"fidelity.policies[].command",
	"fidelity.policies[].roles",
	"fidelity.policies[].applies_to_public",
	"fidelity.policies[].using",
	"fidelity.policies[].with_check",
	"fidelity.unvalidated_checks",
	"fidelity.unvalidated_checks[].name",
	"fidelity.unvalidated_checks[].def",
	"fidelity.column_statistics_targets",
	"fidelity.column_statistics_targets[].column",
	"fidelity.column_statistics_targets[].target",
	"shadow_fidelity",
	"shadow_fidelity.owner",
	"shadow_fidelity.replica_identity",
	"shadow_fidelity.rls_enabled",
	"shadow_fidelity.rls_forced",
	"shadow_fidelity.comment",
	"shadow_fidelity.tablespace",
	"shadow_fidelity.rel_options",
	"shadow_fidelity.grants",
	"shadow_fidelity.grants[].privilege",
	"shadow_fidelity.grants[].grantee",
	"shadow_fidelity.grants[].public",
	"shadow_fidelity.grants[].grantable",
	"shadow_fidelity.column_grants",
	"shadow_fidelity.column_grants[].column",
	"shadow_fidelity.column_grants[].privilege",
	"shadow_fidelity.column_grants[].grantee",
	"shadow_fidelity.column_grants[].public",
	"shadow_fidelity.column_grants[].grantable",
	"shadow_fidelity.policies",
	"shadow_fidelity.policies[].name",
	"shadow_fidelity.policies[].permissive",
	"shadow_fidelity.policies[].command",
	"shadow_fidelity.policies[].roles",
	"shadow_fidelity.policies[].applies_to_public",
	"shadow_fidelity.policies[].using",
	"shadow_fidelity.policies[].with_check",
	"shadow_fidelity.unvalidated_checks",
	"shadow_fidelity.unvalidated_checks[].name",
	"shadow_fidelity.unvalidated_checks[].def",
	"shadow_fidelity.column_statistics_targets",
	"shadow_fidelity.column_statistics_targets[].column",
	"shadow_fidelity.column_statistics_targets[].target",
	"copy_columns",
}

// A checkpoint stores the built shadow as JSON and a resume decodes it back
// into a Proof to compare with what InspectShadow found. The table carries
// an identity column, storage parameters, a table grant, a column grant, a
// policy, a NOT VALID check, and a column statistics target, so every
// nested type is present in the
// encoding and the key-path walk sees each of its fields; Go's encoder and
// decoder agree by field name when a tag is missing, so equality of the two
// proofs alone would not notice a lost tag.
func TestBuiltShadowJSONRoundTripsAsTheProofInspectionRederives(t *testing.T) {
	f, reader := newShadowFixtureWithRole(t)
	f.exec(t, `
		CREATE TABLE %s.orders (
			id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			tenant text NOT NULL,
			qty integer NOT NULL,
			note text
		) WITH (fillfactor = 70)`)
	f.exec(t, `COMMENT ON TABLE %s.orders IS 'customer orders'`)
	f.exec(t, `ALTER TABLE %s.orders ADD CONSTRAINT qty_positive CHECK (qty > 0) NOT VALID`)
	f.exec(t, `ALTER TABLE %s.orders ALTER COLUMN tenant SET STATISTICS 500`)
	f.exec(t, `GRANT SELECT ON %s.orders TO `+pgx.Identifier{reader}.Sanitize())
	f.exec(t, `GRANT UPDATE (note) ON %s.orders TO `+pgx.Identifier{reader}.Sanitize())
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
	assert.Equal(t, built.ShadowFidelity(), proof.ShadowFidelity)
	assert.Equal(t, built.CopyColumns(), proof.CopyColumns)
	require.NotEmpty(t, proof.IdentityColumns, "the fixture populates the identity handoff")
	require.NotEmpty(t, proof.Fidelity.RelOptions, "the fixture populates the storage parameters")
	require.NotEmpty(t, proof.Fidelity.Grants, "the fixture populates the table grants")
	require.NotEmpty(t, proof.Fidelity.ColumnGrants, "the fixture populates the column grants")
	require.NotEmpty(t, proof.Fidelity.Policies, "the fixture populates the policies")
	require.NotEmpty(t, proof.Fidelity.UnvalidatedChecks, "the fixture populates the unvalidated checks")
	require.NotEmpty(t, proof.Fidelity.ColumnStatisticsTargets, "the fixture populates the column statistics targets")
	assert.Equal(t, proof.Fidelity, proof.ShadowFidelity, "a type change leaves the shadow's metadata equal to the source's")

	encoded, err := json.Marshal(built)
	require.NoError(t, err)
	var decoded schemachange.Proof
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	assert.Equal(t, proof, decoded, "the checkpoint decodes to the proof the build returned")

	inspected, err := schemachange.InspectShadow(t.Context(), f.pool, lock, target, schemachange.Options{})
	require.NoError(t, err)
	assert.Equal(t, inspected.Proof(), decoded, "the resume comparison: inspection re-derives the checkpointed proof")

	var tree any
	require.NoError(t, json.Unmarshal(encoded, &tree))
	want := append([]string(nil), proofKeyPaths...)
	sort.Strings(want)
	assert.Equal(t, want, jsonKeyPaths(tree), "the checkpoint's wire shape is the Proof's field set at every depth")
}

// jsonKeyPaths lists every object key reachable in a decoded JSON value,
// dotted by depth, with `[]` for the elements of an array, sorted and
// without duplicates.
func jsonKeyPaths(v any) []string {
	seen := make(map[string]struct{})
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch node := v.(type) {
		case map[string]any:
			for key, child := range node {
				path := key
				if prefix != "" {
					path = prefix + "." + key
				}
				seen[path] = struct{}{}
				walk(path, child)
			}
		case []any:
			for _, child := range node {
				walk(prefix+"[]", child)
			}
		}
	}
	walk("", v)
	paths := make([]string, 0, len(seen))
	for path := range seen {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}
