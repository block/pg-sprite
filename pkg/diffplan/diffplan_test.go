package diffplan_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/statement"
)

// Plan validates its inputs before touching the database: a nil pool proves
// the guards fire first (no connection is ever dialed).
func TestPlanRejectsEmptySchema(t *testing.T) {
	ds, err := statement.ParseDesired("CREATE TABLE events (id bigint PRIMARY KEY)")
	require.NoError(t, err)
	_, err = diffplan.Plan(t.Context(), nil, diffplan.Request{Desired: ds})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "schema name is required")
}

// The zero DesiredSchema — the only one a caller can build without
// statement.ParseDesired — is refused, so every plan that reaches the
// database went through the parse-boundary admission rules.
func TestPlanRejectsDesiredStateWithoutTable(t *testing.T) {
	_, err := diffplan.Plan(t.Context(), nil, diffplan.Request{Schema: "public"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "names no table")
}
