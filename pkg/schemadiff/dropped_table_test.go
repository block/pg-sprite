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

// A foreign key of another table that references a column is listed once,
// by its "referenced by" line, not again under the column.
func TestDroppedTableDefinitionListsAnIncomingForeignKeyOnce(t *testing.T) {
	assert.Equal(t, `table regions
column id: attnum 1: bigint NOT NULL
  with constraint regions_pkey: PRIMARY KEY (id)
constraint regions_pkey: PRIMARY KEY (id)
referenced by warehouses.warehouses_region_id_fkey
`, DroppedTableDefinition(Model{
		Table: "regions",
		Columns: []Column{{Name: "id", AttNum: 1, Type: "bigint", NotNull: true, Dependents: []Dependent{
			{Kind: "constraint", Name: "constraint warehouses_region_id_fkey on table warehouses", Def: "FOREIGN KEY (region_id) REFERENCES regions(id)", Referencing: true},
			{Kind: "constraint", Name: "regions_pkey", Def: "PRIMARY KEY (id)"},
		}}},
		Constraints:  []Constraint{{Name: "regions_pkey", Def: "PRIMARY KEY (id)"}},
		ReferencedBy: []string{"warehouses.warehouses_region_id_fkey"},
	}))
}

// Row security, each policy, and each object that goes with the whole table
// are part of the definition, so a change to any of them shows.
func TestDroppedTableDefinitionListsRowSecurityAndTableDependents(t *testing.T) {
	using := "(owner = CURRENT_USER)"
	assert.Equal(t, `table notes
with trigger notes_audit on table notes: CREATE TRIGGER notes_audit BEFORE INSERT ON notes FOR EACH ROW EXECUTE FUNCTION audit()
with publication of table notes in publication feed
row security enabled
policy notes_owner: permissive for r to app using "(owner = CURRENT_USER)" with check none comment none
`, DroppedTableDefinition(Model{
		Table: "notes",
		TableDependents: []Dependent{
			{Name: "trigger notes_audit on table notes", Def: "CREATE TRIGGER notes_audit BEFORE INSERT ON notes FOR EACH ROW EXECUTE FUNCTION audit()"},
			{Name: "publication of table notes in publication feed"},
		},
		RowSecurity: RowSecurity{Enabled: true, Policies: []Policy{
			{Name: "notes_owner", Command: PolicySelect, Permissive: true, Roles: []string{"app"}, Using: &using},
		}},
	}))
}
