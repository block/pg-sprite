package schemadiff

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The definition a reviewed table drop carries names every fact about the
// table that the drop removes or that changes what it reaches, one per
// line in a fixed order, so a reviewer reads what goes and execution can
// compare it line for line.
func TestDroppedTableDefinition(t *testing.T) {
	m := Model{
		Table:               "events",
		Unlogged:            true,
		PartitionKey:        "RANGE (at)",
		InheritanceChildren: []string{"app.events_archive"},
		Options:             []string{"fillfactor=70"},
		Columns: []Column{
			{Name: "id", AttNum: 1, Type: "bigint", Identity: IdentityAlways, NotNull: true,
				Dependents: []Dependent{{Kind: "identity sequence", Name: "events_id_seq", Def: "start 1"}}},
			{Name: "at", AttNum: 2, Type: "date"},
		},
		Constraints:  []Constraint{{Name: "events_pkey", Def: "PRIMARY KEY (id, at)"}},
		Indexes:      []Index{{Name: "events_at_idx", Def: "CREATE INDEX events_at_idx ON ONLY events USING btree (at)"}},
		ReferencedBy: []string{"tickets.tickets_event_fkey"},
	}
	assert.Equal(t, `table events
unlogged
partition by RANGE (at)
inherited by app.events_archive
option fillfactor=70
column id: attnum 1: bigint GENERATED ALWAYS AS IDENTITY NOT NULL
  with identity sequence events_id_seq: start 1
column at: attnum 2: date
constraint events_pkey: PRIMARY KEY (id, at)
index events_at_idx: CREATE INDEX events_at_idx ON ONLY events USING btree (at)
referenced by tickets.tickets_event_fkey
`, DroppedTableDefinition(m))
}

// A plain table's definition lists only its columns and keys.
func TestDroppedTableDefinitionOfAPlainTable(t *testing.T) {
	assert.Equal(t, `table events
column id: attnum 0: bigint NOT NULL
column name: attnum 0: character varying(50) NOT NULL
constraint events_pkey: PRIMARY KEY (id)
`, DroppedTableDefinition(Model{
		Table: "events",
		Columns: []Column{
			{Name: "id", Type: "bigint", NotNull: true},
			{Name: "name", Type: "character varying(50)", NotNull: true},
		},
		Constraints: []Constraint{{Name: "events_pkey", Def: "PRIMARY KEY (id)"}},
	}))
}
