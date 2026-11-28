package executor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fkDecisionSpec is the foreign key the decision tests ask for:
// events.account_id REFERENCES app.accounts (id), every rule at its default.
var fkDecisionSpec = partitionedFKSpec{
	name: "events_account_fk", columns: []string{"account_id"},
	refSchema: "app", refTable: "accounts", refColumns: []string{"id"},
	match: "s", onUpdate: "a", onDelete: "a",
}

// fkDecisionInput is the resolved foreign key and the leaf the decision
// tests read: the leaf's account_id is attribute 2, and the referenced
// accounts.id is attribute 1 of table 500.
func fkDecisionInput() (resolvedFK, partitionNode) {
	fk := resolvedFK{
		spec:        fkDecisionSpec,
		ref:         fkReference{oid: 500, schema: "app", table: "accounts", attnums: []int16{1}},
		serverMajor: 17,
	}
	return fk, partitionNode{oid: 700, parentOID: 600, schema: "app", name: "events_2026_01", relkind: "r"}
}

// requestedFK is a catalog foreign key of the requested definition on the
// decision tests' leaf.
func requestedFK(oid uint32, name string, validated bool) catalogFK {
	return catalogFK{
		oid: oid, name: name, refOID: 500, key: []int16{2}, refKey: []int16{1},
		match: "s", onUpdate: "a", onDelete: "a", validated: validated,
	}
}

// The resume decision reads one snapshot of the leaf and names the step that
// brings it to the requested foreign key. A foreign key of the requested
// definition is reused whatever its name; the derived name matters only when
// there is none.
func TestDecideLeafForeignKey(t *testing.T) {
	fk, node := fkDecisionInput()
	const derived = "events_account_fk_p700"
	onDeleteCascade := requestedFK(41, "events_jan_cascade_fk", true)
	onDeleteCascade.onDelete = "c"
	withSetColumns := requestedFK(42, "events_jan_set_null_fk", true)
	withSetColumns.onDelete, withSetColumns.deleteSetColumns = "n", []int16{2}

	tests := []struct {
		name  string
		facts leafFKFacts
		want  leafFKPlan
	}{
		{
			name:  "no foreign key: add one under the derived name",
			facts: leafFKFacts{attnums: []int16{2}},
			want:  leafFKPlan{state: leafFKAddNeeded},
		},
		{
			name:  "an earlier run's validated foreign key is done",
			facts: leafFKFacts{attnums: []int16{2}, foreignKeys: []catalogFK{requestedFK(40, derived, true)}, nameHeld: true},
			want:  leafFKPlan{state: leafFKDone, fk: requestedFK(40, derived, true)},
		},
		{
			name:  "an earlier run's NOT VALID foreign key is validated",
			facts: leafFKFacts{attnums: []int16{2}, foreignKeys: []catalogFK{requestedFK(40, derived, false)}, nameHeld: true},
			want:  leafFKPlan{state: leafFKValidatePending, fk: requestedFK(40, derived, false)},
		},
		{
			name:  "an operator's matching foreign key under their own name is reused",
			facts: leafFKFacts{attnums: []int16{2}, foreignKeys: []catalogFK{requestedFK(40, "jan_account_fk", true)}},
			want:  leafFKPlan{state: leafFKDone, fk: requestedFK(40, "jan_account_fk", true)},
		},
		{
			// The parent's statement would adopt neither: each has an ON
			// DELETE other than the requested NO ACTION.
			name:  "foreign keys of another definition are left alone",
			facts: leafFKFacts{attnums: []int16{2}, foreignKeys: []catalogFK{onDeleteCascade, withSetColumns}},
			want:  leafFKPlan{state: leafFKAddNeeded},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decideLeafForeignKey(node, derived, fk, tt.facts)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// Each refusal is typed, and none of them depends on the order the catalog
// lists the leaf's constraints in.
func TestDecideLeafForeignKeyRefusals(t *testing.T) {
	fk, node := fkDecisionInput()
	const derived = "events_account_fk_p700"
	adopted := requestedFK(40, "jan_account_fk", true)
	adopted.parentOID = 900

	tests := []struct {
		name  string
		facts leafFKFacts
		want  error
	}{
		{
			// The parent's statement would adopt one of them and leave the
			// other, and which one would be the server's choice.
			name: "two foreign keys of the requested definition",
			facts: leafFKFacts{attnums: []int16{2}, foreignKeys: []catalogFK{
				requestedFK(40, "jan_account_fk", true), requestedFK(41, derived, false),
			}},
			want: ErrPartitionedForeignKeyConflict,
		},
		{
			name:  "a matching foreign key already adopted by a parent's",
			facts: leafFKFacts{attnums: []int16{2}, foreignKeys: []catalogFK{adopted}},
			want:  ErrPartitionedForeignKeyConflict,
		},
		{
			name:  "the derived name held by another constraint",
			facts: leafFKFacts{attnums: []int16{2}, nameHeld: true},
			want:  ErrPartitionedForeignKeyNameTaken,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decideLeafForeignKey(node, derived, fk, tt.facts)
			require.ErrorIs(t, err, tt.want)
		})
	}

	// The server compares everything but the ON DELETE column list when it
	// adopts, so for a requested ON DELETE SET NULL a foreign key with a
	// column list could be adopted in place of the step's, and is refused
	// whether or not the requested one is there too.
	setNull := fk
	setNull.spec.onDelete = "n"
	exact := requestedFK(40, derived, true)
	exact.onDelete = "n"
	listed := requestedFK(41, "a_jan_account_fk", true)
	listed.onDelete, listed.deleteSetColumns = "n", []int16{2}
	for name, fks := range map[string][]catalogFK{
		"alone":                    {listed},
		"beside the requested one": {exact, listed},
	} {
		t.Run("a column-list foreign key "+name, func(t *testing.T) {
			_, err := decideLeafForeignKey(node, derived, setNull, leafFKFacts{attnums: []int16{2}, foreignKeys: fks, nameHeld: true})
			require.ErrorIs(t, err, ErrPartitionedForeignKeyConflict)
		})
	}
}

// The parent adopts a partition's foreign key only when every rule matches,
// so each one is part of the definition the step recognises.
func TestCatalogFKIsRequestedComparesEveryRule(t *testing.T) {
	fk, _ := fkDecisionInput()
	leafAttnums := []int16{2}
	require.True(t, requestedFK(40, "fk", true).isRequested(fk.spec, leafAttnums, fk.ref))

	tests := map[string]func(k *catalogFK){
		"another referenced table":      func(k *catalogFK) { k.refOID = 501 },
		"other referencing columns":     func(k *catalogFK) { k.key = []int16{3} },
		"other referenced columns":      func(k *catalogFK) { k.refKey = []int16{2} },
		"MATCH FULL":                    func(k *catalogFK) { k.match = "f" },
		"ON UPDATE CASCADE":             func(k *catalogFK) { k.onUpdate = "c" },
		"ON DELETE RESTRICT":            func(k *catalogFK) { k.onDelete = "r" },
		"an ON DELETE column list":      func(k *catalogFK) { k.deleteSetColumns = []int16{2} },
		"NOT ENFORCED":                  func(k *catalogFK) { k.notEnforced = true },
		"DEFERRABLE":                    func(k *catalogFK) { k.deferrable = true },
		"DEFERRABLE INITIALLY DEFERRED": func(k *catalogFK) { k.deferrable, k.initiallyDeferred = true, true },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			k := requestedFK(40, "fk", true)
			change(&k)
			assert.False(t, k.isRequested(fk.spec, leafAttnums, fk.ref))
		})
	}
}

// The leaf's statements carry every rule of the spec spelled out, each
// identifier quoted, so the leaf's foreign key and the parent's share the
// definition the parent's statement compares.
func TestLeafFKStatements(t *testing.T) {
	spec := partitionedFKSpec{
		name: "events_account_fk", columns: []string{"account_id", "Region"},
		refSchema: "app", refTable: "accounts", refColumns: []string{"id", "Region"},
		match: "f", onUpdate: "r", onDelete: "c", deferrable: true, initiallyDeferred: true,
	}
	require.NoError(t, spec.validate())
	node := partitionNode{oid: 700, schema: "app", name: "events_2026_01", relkind: "r"}

	assert.Equal(t,
		`ALTER TABLE "app"."events_2026_01" ADD CONSTRAINT "events_account_fk_p700" `+
			`FOREIGN KEY ("account_id", "Region") REFERENCES "app"."accounts" ("id", "Region") `+
			`MATCH FULL ON UPDATE RESTRICT ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED NOT VALID`,
		leafFKAddSQL(spec, node, derivedPartitionIndexName(spec.name, node.oid)))
	assert.Equal(t,
		`ALTER TABLE "app"."events_2026_01" VALIDATE CONSTRAINT "events_account_fk_p700"`,
		leafFKValidateSQL(node, "events_account_fk_p700"))

	spec.onDelete, spec.deleteSetColumns = "d", []string{"Region"}
	require.NoError(t, spec.validate())
	assert.Equal(t,
		`ALTER TABLE "app"."events_2026_01" ADD CONSTRAINT "events_account_fk_p700" `+
			`FOREIGN KEY ("account_id", "Region") REFERENCES "app"."accounts" ("id", "Region") `+
			`MATCH FULL ON UPDATE RESTRICT ON DELETE SET DEFAULT ("Region") DEFERRABLE INITIALLY DEFERRED NOT VALID`,
		leafFKAddSQL(spec, node, derivedPartitionIndexName(spec.name, node.oid)))
}

// An ON DELETE column list is compared as the set of columns it names, by
// each table's own attribute numbers: a partition may number its columns
// differently from its parent, and the server keeps the list in the order
// it was written.
func TestCatalogFKIsRequestedComparesTheDeleteColumnList(t *testing.T) {
	fk, _ := fkDecisionInput()
	fk.spec.columns, fk.spec.refColumns = []string{"account_id", "region"}, []string{"id", "region"}
	fk.spec.onDelete, fk.spec.deleteSetColumns = "n", []string{"region", "account_id"}
	fk.ref.attnums = []int16{1, 2}
	// On this leaf account_id is attribute 5 and region attribute 3.
	leafAttnums := []int16{5, 3}
	k := requestedFK(40, "fk", true)
	k.key, k.refKey, k.onDelete = []int16{5, 3}, []int16{1, 2}, "n"

	for name, tt := range map[string]struct {
		list []int16
		want bool
	}{
		"the same columns in the written order": {list: []int16{3, 5}, want: true},
		"the same columns in another order":     {list: []int16{5, 3}, want: true},
		"no list":                               {list: nil, want: false},
		"one of the two columns":                {list: []int16{3}, want: false},
		"other columns":                         {list: []int16{2, 1}, want: false},
	} {
		t.Run(name, func(t *testing.T) {
			k.deleteSetColumns = tt.list
			assert.Equal(t, tt.want, k.isRequested(fk.spec, leafAttnums, fk.ref))
			assert.True(t, k.isAdoptable(fk.spec, leafAttnums, fk.ref), "the server adopts it whatever its list")
		})
	}
}

// A spec the step could not render faithfully is refused before anything
// runs.
func TestPartitionedFKSpecValidate(t *testing.T) {
	tests := map[string]func(s *partitionedFKSpec){
		"no name":                           func(s *partitionedFKSpec) { s.name = "" },
		"fewer referenced columns":          func(s *partitionedFKSpec) { s.refColumns = nil },
		"an unqualified referenced table":   func(s *partitionedFKSpec) { s.refSchema = "" },
		"MATCH PARTIAL":                     func(s *partitionedFKSpec) { s.match = "p" },
		"an unknown action":                 func(s *partitionedFKSpec) { s.onDelete = "x" },
		"INITIALLY DEFERRED not DEFERRABLE": func(s *partitionedFKSpec) { s.initiallyDeferred = true },
		"a repeated column": func(s *partitionedFKSpec) {
			s.columns, s.refColumns = []string{"a", "a"}, []string{"x", "y"}
		},
		"an ON DELETE column list on NO ACTION": func(s *partitionedFKSpec) {
			s.deleteSetColumns = []string{"account_id"}
		},
		"an ON DELETE column outside the foreign key": func(s *partitionedFKSpec) {
			s.onDelete, s.deleteSetColumns = "n", []string{"note"}
		},
		"an ON DELETE column named twice": func(s *partitionedFKSpec) {
			s.onDelete, s.deleteSetColumns = "n", []string{"account_id", "account_id"}
		},
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			s := fkDecisionSpec
			change(&s)
			assert.ErrorIs(t, s.validate(), ErrInvariantViolation)
		})
	}
}
