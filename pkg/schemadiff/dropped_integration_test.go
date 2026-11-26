package schemadiff_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/internal/testutil/modeltest"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemadiff"
)

// The definition a drop carries is read two ways that must agree: by Diff
// from the table it introspects, and by ReadDroppedObject from the catalog
// when execution checks the drop under the table's lock. A reviewed drop
// refuses on any difference, so the two renderings are identical for every
// kind of object a plan drops.
func TestReadDroppedObjectMatchesTheDropADiffPlans(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.t (
		  id bigint PRIMARY KEY,
		  note text COLLATE "C" NOT NULL DEFAULT 'none',
		  qty integer NOT NULL,
		  CONSTRAINT t_qty_positive CHECK (qty > 0)
		);
		CREATE INDEX t_note_idx ON %[1]s.t (note)`, schema))
	require.NoError(t, err)

	for _, tc := range []struct {
		kind schemadiff.DroppedKind
		name string
	}{
		{schemadiff.DroppedIndex, "t_note_idx"},
		{schemadiff.DroppedConstraint, "t_qty_positive"},
		{schemadiff.DroppedColumn, "note"},
		{schemadiff.DroppedNotNull, "qty"},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			planned := modeltest.PlannedDrop(t, pool, schema, "t", tc.kind, tc.name)
			live, found, err := schemadiff.ReadDroppedObject(t.Context(), pool, schema, "t", tc.kind, tc.name)
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, planned, live)
		})
	}

	_, found, err := schemadiff.ReadDroppedObject(t.Context(), pool, schema, "t", schemadiff.DroppedIndex, "no_such_idx")
	require.NoError(t, err)
	assert.False(t, found, "an object the table does not have is not found")
	_, found, err = schemadiff.ReadDroppedObject(t.Context(), pool, schema, "no_such_table", schemadiff.DroppedColumn, "note")
	require.NoError(t, err)
	assert.False(t, found, "a table that does not exist has no object to find")
}

// A dropped column lists everything that goes with it, rendered with the
// definition that tells an object from another that took its name: indexes,
// constraints including another table's foreign key, owned and identity
// sequences, and any other dependent object such as extended statistics.
func TestDroppedColumnListsEveryDependent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  string
		column string
		want   []string
	}{
		{
			name:   "index",
			setup:  `CREATE INDEX t_note_idx ON %[1]s.t (note)`,
			column: "note",
			want:   []string{"index t_note_idx: CREATE INDEX t_note_idx ON SCHEMA.t USING btree (note)"},
		},
		{
			name:   "check constraint",
			setup:  `ALTER TABLE %[1]s.t ADD CONSTRAINT t_note_check CHECK (note <> '')`,
			column: "note",
			want:   []string{"constraint t_note_check: CHECK ((note <> ''::text))"},
		},
		{
			name: "foreign key of another table",
			setup: `CREATE UNIQUE INDEX t_code_key ON %[1]s.t (code);
			        CREATE TABLE %[1]s.child (ref text REFERENCES %[1]s.t (code))`,
			column: "code",
			want: []string{
				"constraint constraint child_ref_fkey on table child: FOREIGN KEY (ref) REFERENCES t(code)",
				"index t_code_key: CREATE UNIQUE INDEX t_code_key ON SCHEMA.t USING btree (code)",
			},
		},
		{
			name:   "owned sequence",
			setup:  `CREATE SEQUENCE %[1]s.t_note_seq OWNED BY %[1]s.t.note`,
			column: "note",
			want:   []string{"owned sequence SCHEMA.t_note_seq: bigint START WITH 1 INCREMENT BY 1 MINVALUE 1 MAXVALUE 9223372036854775807 CACHE 1 NO CYCLE"},
		},
		{
			name:   "identity sequence",
			setup:  `ALTER TABLE %[1]s.t ADD COLUMN seq integer GENERATED ALWAYS AS IDENTITY`,
			column: "seq",
			want:   []string{"identity sequence SCHEMA.t_seq_seq: integer START WITH 1 INCREMENT BY 1 MINVALUE 1 MAXVALUE 2147483647 CACHE 1 NO CYCLE"},
		},
		{
			name:   "extended statistics",
			setup:  `CREATE STATISTICS %[1]s.t_stats ON note, code FROM %[1]s.t`,
			column: "note",
			want:   []string{"other statistics object t_stats"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
			require.NoError(t, err)
			defer pool.Close()
			schema := testutil.NewSchema(t, pool)
			_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %[1]s.t (id bigint PRIMARY KEY, note text, code text)`, schema))
			require.NoError(t, err)
			_, err = pool.Exec(t.Context(), fmt.Sprintf(tc.setup, schema))
			require.NoError(t, err)

			drop := modeltest.PlannedDrop(t, pool, schema, "t", schemadiff.DroppedColumn, tc.column)
			want := make([]string, len(tc.want))
			for i, line := range tc.want {
				want[i] = strings.ReplaceAll(line, "SCHEMA", schema)
			}
			assert.Equal(t, want, drop.Dependents)
		})
	}
}
