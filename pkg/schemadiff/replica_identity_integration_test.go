package schemadiff_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemadiff"
)

// Introspection reports what the replica identity indexes of a table and of
// every table below it, partitions and inheritance children alike, are
// built on: the index on the table whose drop would remove one, the primary
// key or unique constraint that owns it, and every column it covers,
// INCLUDE columns too, and apart from those the columns of the table's own
// identity index. A table
// whose identity is not an index reports nothing, and the facts stay out of
// the model's JSON, so no digest of the model moves with them.
func TestIntrospectReplicaIdentity(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	cases := []struct {
		name  string
		setup string
		table string
		want  schemadiff.ReplicaIdentityKeys
	}{
		{
			name:  "default identity",
			setup: `CREATE TABLE %[1]s.t (id bigint PRIMARY KEY, ref text NOT NULL UNIQUE)`,
			table: "t",
		},
		{
			name: "full identity",
			setup: `CREATE TABLE %[1]s.t (id bigint PRIMARY KEY);
				ALTER TABLE %[1]s.t REPLICA IDENTITY FULL`,
			table: "t",
		},
		{
			name: "a plain unique index",
			setup: `CREATE TABLE %[1]s.t (id bigint, tenant bigint NOT NULL, ref text NOT NULL);
				CREATE UNIQUE INDEX t_ref_idx ON %[1]s.t (tenant, ref);
				ALTER TABLE %[1]s.t REPLICA IDENTITY USING INDEX t_ref_idx`,
			table: "t",
			want: schemadiff.ReplicaIdentityKeys{
				Indexes:    []string{"t_ref_idx"},
				Columns:    []string{"ref", "tenant"},
				OwnColumns: []string{"ref", "tenant"},
			},
		},
		{
			name: "the primary key",
			setup: `CREATE TABLE %[1]s.t (id bigint, tenant bigint, CONSTRAINT t_pk PRIMARY KEY (tenant, id));
				ALTER TABLE %[1]s.t REPLICA IDENTITY USING INDEX t_pk`,
			table: "t",
			want: schemadiff.ReplicaIdentityKeys{
				Indexes:     []string{"t_pk"},
				Constraints: []string{"t_pk"},
				Columns:     []string{"id", "tenant"},
				OwnColumns:  []string{"id", "tenant"},
			},
		},
		{
			name: "an index with INCLUDE columns",
			setup: `CREATE TABLE %[1]s.t (id bigint, ref text NOT NULL, note text);
				CREATE UNIQUE INDEX t_ref_idx ON %[1]s.t (ref) INCLUDE (note);
				ALTER TABLE %[1]s.t REPLICA IDENTITY USING INDEX t_ref_idx`,
			table: "t",
			want: schemadiff.ReplicaIdentityKeys{
				Indexes:    []string{"t_ref_idx"},
				Columns:    []string{"note", "ref"},
				OwnColumns: []string{"note", "ref"},
			},
		},
		{
			name: "the primary key a self-referencing foreign key uses",
			setup: `CREATE TABLE %[1]s.t (id bigint PRIMARY KEY, parent bigint REFERENCES %[1]s.t (id));
				ALTER TABLE %[1]s.t REPLICA IDENTITY USING INDEX t_pkey`,
			table: "t",
			want: schemadiff.ReplicaIdentityKeys{
				Indexes:     []string{"t_pkey"},
				Constraints: []string{"t_pkey"},
				Columns:     []string{"id"},
				OwnColumns:  []string{"id"},
			},
		},
		{
			name: "an inheritance child's identity on a column it inherits",
			setup: `CREATE TABLE %[1]s.parent (id bigint, ref text NOT NULL);
				CREATE TABLE %[1]s.child () INHERITS (%[1]s.parent);
				CREATE UNIQUE INDEX child_ref_idx ON %[1]s.child (ref);
				ALTER TABLE %[1]s.child REPLICA IDENTITY USING INDEX child_ref_idx`,
			table: "parent",
			want: schemadiff.ReplicaIdentityKeys{
				Columns: []string{"ref"},
			},
		},
		{
			name: "a partition's identity attached under the parent's index",
			setup: `CREATE TABLE %[1]s.p (id bigint NOT NULL, ref text NOT NULL) PARTITION BY RANGE (id);
				CREATE TABLE %[1]s.p_1 PARTITION OF %[1]s.p FOR VALUES FROM (0) TO (100);
				ALTER TABLE %[1]s.p ADD CONSTRAINT p_ref_key UNIQUE (id, ref);
				ALTER TABLE %[1]s.p_1 REPLICA IDENTITY USING INDEX p_1_id_ref_key`,
			table: "p",
			want: schemadiff.ReplicaIdentityKeys{
				Indexes:     []string{"p_ref_key"},
				Constraints: []string{"p_ref_key"},
				Columns:     []string{"id", "ref"},
			},
		},
		{
			name: "a partition's identity on an index of its own",
			setup: `CREATE TABLE %[1]s.p (id bigint NOT NULL, ref text NOT NULL) PARTITION BY RANGE (id);
				CREATE TABLE %[1]s.p_1 PARTITION OF %[1]s.p FOR VALUES FROM (0) TO (100);
				CREATE UNIQUE INDEX p_1_ref_idx ON %[1]s.p_1 (ref);
				ALTER TABLE %[1]s.p_1 REPLICA IDENTITY USING INDEX p_1_ref_idx`,
			table: "p",
			want: schemadiff.ReplicaIdentityKeys{
				Columns: []string{"ref"},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			schema := testutil.NewSchema(t, pool)
			_, err := pool.Exec(t.Context(), fmt.Sprintf(c.setup, schema))
			require.NoError(t, err)
			m, err := schemadiff.Introspect(t.Context(), pool, schema, c.table)
			require.NoError(t, err)
			assert.Equal(t, c.want, m.ReplicaIdentity)

			withoutFacts := m
			withoutFacts.ReplicaIdentity = schemadiff.ReplicaIdentityKeys{}
			assert.Equal(t, mustJSON(t, withoutFacts), mustJSON(t, m), "the facts are not part of the model's JSON")
		})
	}
}

func mustJSON(t *testing.T, m schemadiff.Model) string {
	t.Helper()
	encoded, err := json.Marshal(m)
	require.NoError(t, err)
	return string(encoded)
}
