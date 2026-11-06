package schemadiff_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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
// identity index. Under REPLICA IDENTITY DEFAULT the primary key is the
// identity index once a publication publishes the table's updates or
// deletes, whether it lists the table, a partitioned table above it, or the
// table's schema. A table whose identity is FULL, or DEFAULT with no
// publication publishing its updates or deletes, reports nothing, and the
// facts stay out of the model's JSON, so no digest of the model moves with
// them.
func TestIntrospectReplicaIdentity(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	major, err := dbconn.ServerMajor(t.Context(), pool)
	require.NoError(t, err)

	cases := []struct {
		name     string
		setup    string
		table    string
		minMajor int
		want     schemadiff.ReplicaIdentityKeys
	}{
		{
			name:  "default identity no publication publishes",
			setup: `CREATE TABLE %[1]s.t (id bigint PRIMARY KEY, ref text NOT NULL UNIQUE)`,
			table: "t",
		},
		{
			name: "default identity on a table a publication publishes",
			setup: `CREATE TABLE %[1]s.t (id bigint, tenant bigint, ref text NOT NULL UNIQUE, CONSTRAINT t_pk PRIMARY KEY (tenant, id));
				CREATE PUBLICATION %[1]s_pub FOR TABLE %[1]s.t`,
			table: "t",
			want: schemadiff.ReplicaIdentityKeys{
				Indexes:     []string{"t_pk"},
				Constraints: []string{"t_pk"},
				Columns:     []string{"id", "tenant"},
				OwnColumns:  []string{"id", "tenant"},
			},
		},
		{
			name: "default identity on a table published for deletes only",
			setup: `CREATE TABLE %[1]s.t (id bigint PRIMARY KEY);
				CREATE PUBLICATION %[1]s_pub FOR TABLE %[1]s.t WITH (publish = 'insert, delete')`,
			table: "t",
			want: schemadiff.ReplicaIdentityKeys{
				Indexes:     []string{"t_pkey"},
				Constraints: []string{"t_pkey"},
				Columns:     []string{"id"},
				OwnColumns:  []string{"id"},
			},
		},
		{
			name: "default identity on a table published for inserts only",
			setup: `CREATE TABLE %[1]s.t (id bigint PRIMARY KEY);
				CREATE PUBLICATION %[1]s_pub FOR TABLE %[1]s.t WITH (publish = 'insert, truncate')`,
			table: "t",
		},
		{
			name: "default identity on a table its schema's publication publishes",
			setup: `CREATE TABLE %[1]s.t (id bigint PRIMARY KEY);
				CREATE PUBLICATION %[1]s_pub FOR TABLES IN SCHEMA %[1]s`,
			table:    "t",
			minMajor: 15,
			want: schemadiff.ReplicaIdentityKeys{
				Indexes:     []string{"t_pkey"},
				Constraints: []string{"t_pkey"},
				Columns:     []string{"id"},
				OwnColumns:  []string{"id"},
			},
		},
		{
			name: "default identity on partitions a publication publishes through their parent",
			setup: `CREATE TABLE %[1]s.p (id bigint, ref text NOT NULL, PRIMARY KEY (id)) PARTITION BY RANGE (id);
				CREATE TABLE %[1]s.p_1 PARTITION OF %[1]s.p FOR VALUES FROM (0) TO (100);
				CREATE PUBLICATION %[1]s_pub FOR TABLE %[1]s.p WITH (publish_via_partition_root = true)`,
			table: "p",
			want: schemadiff.ReplicaIdentityKeys{
				Indexes:     []string{"p_pkey"},
				Constraints: []string{"p_pkey"},
				Columns:     []string{"id"},
			},
		},
		{
			name: "default identity on one partition a publication publishes",
			setup: `CREATE TABLE %[1]s.p (id bigint, ref text NOT NULL, PRIMARY KEY (id)) PARTITION BY RANGE (id);
				CREATE TABLE %[1]s.p_1 PARTITION OF %[1]s.p FOR VALUES FROM (0) TO (100);
				CREATE TABLE %[1]s.p_2 PARTITION OF %[1]s.p FOR VALUES FROM (100) TO (200);
				CREATE PUBLICATION %[1]s_pub FOR TABLE %[1]s.p_2`,
			table: "p",
			want: schemadiff.ReplicaIdentityKeys{
				Indexes:     []string{"p_pkey"},
				Constraints: []string{"p_pkey"},
				Columns:     []string{"id"},
			},
		},
		{
			name: "full identity",
			setup: `CREATE TABLE %[1]s.t (id bigint PRIMARY KEY);
				ALTER TABLE %[1]s.t REPLICA IDENTITY FULL;
				CREATE PUBLICATION %[1]s_pub FOR TABLE %[1]s.t`,
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
			if major < c.minMajor {
				t.Skipf("the case needs PostgreSQL %d or later; server reports %d", c.minMajor, major)
			}
			schema := testutil.NewSchema(t, pool)
			dropPublication(t, pool, schema+"_pub")
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

// dropPublication drops the named publication when the test ends, if the
// test created it. A publication belongs to the database, not to a schema,
// so dropping the test's schema leaves it behind.
func dropPublication(t *testing.T, pool *pgxpool.Pool, name string) {
	t.Helper()
	t.Cleanup(func() {
		// t.Context is cancelled by cleanup time; strip the cancellation.
		_, err := pool.Exec(context.WithoutCancel(t.Context()), "DROP PUBLICATION IF EXISTS "+pgx.Identifier{name}.Sanitize())
		if err != nil {
			t.Logf("drop publication %s: %v", name, err)
		}
	})
}

func mustJSON(t *testing.T, m schemadiff.Model) string {
	t.Helper()
	encoded, err := json.Marshal(m)
	require.NoError(t, err)
	return string(encoded)
}
