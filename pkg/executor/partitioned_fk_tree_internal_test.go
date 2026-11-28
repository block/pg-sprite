package executor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests here read refuseFKTree, the partition tree check the build runs
// under its lock and AdmitPartitionedForeignKey runs before any statement,
// over app.events (OID 1) adding a foreign key to app.accounts (OID 100).
var (
	fkTreeTarget    = indexTarget{tableOID: 1, schema: "app", table: "events"}
	fkTreeReference = fkReference{oid: 100, schema: "app", table: "accounts"}
)

// fkLeaf is a plain-table partition of app.events.
func fkLeaf(oid uint32, name string) partitionNode {
	return partitionNode{oid: oid, parentOID: 1, schema: "app", name: name, relkind: "r"}
}

// A tree of plain partitions, none detaching and none the referenced table,
// is one the build takes.
func TestRefuseFKTreeAdmitsPlainPartitions(t *testing.T) {
	require.NoError(t, refuseFKTree(fkTreeTarget, fkTreeReference, []partitionNode{fkLeaf(11, "events_01"), fkLeaf(12, "events_02")}))
}

// Each shape the build does not take is refused with its own sentinel,
// naming the partition. A tree with several holds the first in the build's
// order: a foreign table, then a pending detach, then the referenced table,
// then a sub-partitioned partition.
func TestRefuseFKTreeRefusesEachShapeWithItsSentinel(t *testing.T) {
	foreign := fkLeaf(13, "events_remote")
	foreign.relkind = "f"
	detaching := fkLeaf(14, "events_old")
	detaching.detachPending = true
	nested := fkLeaf(15, "events_2026")
	nested.relkind = "p"
	referenced := fkLeaf(100, "accounts")

	for name, tc := range map[string]struct {
		tree  []partitionNode
		want  error
		names string
	}{
		"foreign table":      {[]partitionNode{fkLeaf(11, "events_01"), foreign}, ErrForeignTablePartition, "app.events_remote"},
		"detach pending":     {[]partitionNode{fkLeaf(11, "events_01"), detaching}, ErrPartitionDetachPending, "app.events_old"},
		"sub-partitioned":    {[]partitionNode{nested}, ErrPartitionedForeignKeySubPartitioned, "app.events_2026"},
		"referenced table":   {[]partitionNode{referenced}, ErrPartitionedForeignKeyReferencedTable, "app.accounts"},
		"foreign first":      {[]partitionNode{nested, detaching, foreign}, ErrForeignTablePartition, "app.events_remote"},
		"referenced first":   {[]partitionNode{nested, referenced}, ErrPartitionedForeignKeyReferencedTable, "app.accounts"},
		"detach before nest": {[]partitionNode{nested, detaching}, ErrPartitionDetachPending, "app.events_old"},
	} {
		t.Run(name, func(t *testing.T) {
			err := refuseFKTree(fkTreeTarget, fkTreeReference, tc.tree)
			require.ErrorIs(t, err, tc.want)
			assert.Contains(t, err.Error(), tc.names)
		})
	}
}
