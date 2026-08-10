package diffplan_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// Plan validates its inputs before touching the database: a nil pool proves
// the guards fire first (no connection is ever dialed).
func TestPlanRejectsEmptySchema(t *testing.T) {
	ds, err := statement.ParseDesired("CREATE TABLE events (id bigint PRIMARY KEY)")
	require.NoError(t, err)
	_, err = diffplan.Plan(t.Context(), nil, "", ds)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "schema name is required")
}

func TestPlanRejectsDesiredStateWithoutTable(t *testing.T) {
	_, err := diffplan.Plan(t.Context(), nil, "public", statement.DesiredSchema{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "names no table")
}

func TestLiveFactsExtractsColumnTypes(t *testing.T) {
	live := schemadiff.Model{Columns: []schemadiff.Column{
		{Name: "id", Type: "bigint"},
		{Name: "name", Type: "character varying(50)"},
	}}
	facts := diffplan.LiveFacts(live)
	assert.Equal(t, map[string]string{
		"id":   "bigint",
		"name": "character varying(50)",
	}, facts.ColumnTypes)
}
