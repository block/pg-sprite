package schemachange_test

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// The proof a run saves is what a resume decodes, after the checkpoint
// store's jsonb column has stored it by value and rendered it its own way:
// keys reordered, spacing dropped, numbers rewritten as numeric. The proof
// here is a literal at the edges a build can reach — identity sequence
// bounds at both ends of int64, OIDs at the top of uint32, names and
// expressions with quotes, backslashes, and non-ASCII text, a NOT VALID
// check with a nested expression, an empty list beside a populated one —
// so that any value the column would not hand back as it was given shows
// up as an inequality rather than as a resume that refuses its own shadow.
func TestProofSurvivesTheCheckpointStore(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.orders (
			id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			tenant text NOT NULL
		)`)
	store, err := checkpoint.NewStore(f.pool, checkpoint.Options{})
	require.NoError(t, err)
	require.NoError(t, store.Ensure(t.Context()))
	lock := f.lock(t, "orders")

	proof := schemachange.Proof{
		Schema:            f.schema,
		SourceTable:       "orders",
		ShadowTable:       "_pgsprite_orders_new",
		SourceOID:         math.MaxUint32,
		ShadowOID:         math.MaxUint32 - 1,
		SourceFingerprint: "a3f1c9e2b4d5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3f4a5b6c7d8e9f0a1",
		TargetFingerprint: "b4d5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3f4a5b6c7d8e9f0a1a3f1c9e2",
		IdentityColumns: []schemachange.IdentityColumn{{
			Column:         "id",
			Always:         true,
			SequenceSchema: f.schema,
			SequenceName:   "orders_id_seq",
			Options: schemachange.SequenceOptions{
				Start:     math.MinInt64,
				Increment: -1,
				Min:       math.MinInt64,
				Max:       math.MaxInt64,
				Cache:     1,
				Cycle:     true,
			},
		}},
		Fidelity: schemachange.FidelitySnapshot{
			Owner:           `app "owner"`,
			ReplicaIdentity: "f",
			RLSEnabled:      true,
			RLSForced:       false,
			Comment:         "orders — \"customer\" orders\\n with a backslash and 日本語",
			Tablespace:      "",
			RelOptions:      []string{"fillfactor=70", "toast.autovacuum_enabled=false"},
			Grants: []schemachange.Grant{
				{Privilege: "SELECT", Grantee: "", Public: true, Grantable: false},
				{Privilege: "UPDATE", Grantee: "PUBLIC", Public: false, Grantable: true},
			},
			ColumnGrants: []schemachange.ColumnGrant{{Column: "tenant", Grant: schemachange.Grant{Privilege: "REFERENCES", Grantee: "reader"}}},
			Policies: []schemachange.Policy{{
				Name:            "tenant_read",
				Permissive:      false,
				Command:         "r",
				Roles:           []string{"reader", "auditor"},
				AppliesToPublic: false,
				Using:           `(tenant = (current_user)::text)`,
				WithCheck:       "",
			}},
			UnvalidatedChecks:         []schemachange.UnvalidatedConstraint{{Name: "qty_positive", Def: "CHECK ((qty > 0) AND (tenant <> ''::text)) NOT VALID"}},
			ColumnStatisticsTargets:   []schemachange.ColumnStatisticsTarget{{Column: "tenant", Target: 10000}},
			ExtendedStatisticsTargets: []schemachange.ExtendedStatisticsTarget{},
		},
		ShadowFidelity: schemachange.FidelitySnapshot{
			Owner:           `app "owner"`,
			ReplicaIdentity: "d",
			RelOptions:      []string{},
			Grants:          []schemachange.Grant{},
			ColumnGrants:    []schemachange.ColumnGrant{},
			Policies:        []schemachange.Policy{},
		},
		CopyColumns: []string{"id", "tenant", `we"ird`},
	}
	encoded, err := json.Marshal(proof)
	require.NoError(t, err)

	cp := checkpoint.Checkpoint{
		Schema: f.schema, Table: "orders", ShadowTable: proof.ShadowTable,
		SourceFingerprint: proof.SourceFingerprint, TargetFingerprint: proof.TargetFingerprint,
		Phase: checkpoint.PhaseVerifying, ShadowProof: encoded,
	}
	require.NoError(t, store.Save(t.Context(), lock, cp))

	stored, err := store.Inspect(t.Context(), f.schema, "orders")
	require.NoError(t, err)
	var decoded schemachange.Proof
	require.NoError(t, json.Unmarshal(stored.ShadowProof, &decoded))
	assert.Equal(t, proof, decoded, "the proof comes back from jsonb as the value that was saved")

	cp.ShadowProof = stored.ShadowProof
	cp.Phase = checkpoint.PhaseCutover
	require.NoError(t, store.Save(t.Context(), lock, cp), "a Save that carries the proof as the server rendered it is the same proof to the store")
}
