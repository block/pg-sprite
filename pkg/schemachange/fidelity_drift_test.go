package schemachange

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A snapshot compared with itself has no drift, and a snapshot that differs
// in several facts names each of them in field order — the refusal detail
// tells the operator what moved, not just that something did.
func TestFidelityDriftNamesExactlyTheFactsThatDiffer(t *testing.T) {
	want := FidelitySnapshot{
		Owner:           "app",
		ReplicaIdentity: "d",
		Comment:         "customer orders",
		RelOptions:      []string{"fillfactor=70"},
		Grants:          []Grant{{Privilege: "SELECT", Grantee: "reader"}},
		Policies:        []Policy{{Name: "tenant_read", Permissive: true, Command: "r", Roles: []string{"reader"}, Using: "(tenant = CURRENT_USER)"}},
	}
	same, err := fidelityDrift(want, want)
	require.NoError(t, err)
	assert.Empty(t, same)

	have := want
	have.Comment = ""
	have.Grants = []Grant{{Privilege: "SELECT", Grantee: "reader"}, {Privilege: "INSERT", Grantee: "writer"}}
	have.ColumnStatisticsTargets = []ColumnStatisticsTarget{{Column: "tenant", Target: 500}}
	drifted, err := fidelityDrift(have, want)
	require.NoError(t, err)
	assert.Equal(t, []string{"comment", "grants", "column_statistics_targets"}, drifted)
}

// A policy whose role list changed drifts, although the policy's name and
// every scalar field are unchanged: the comparison reaches into nested
// slices rather than stopping at the entry count.
func TestFidelityDriftSeesInsideNestedValues(t *testing.T) {
	want := FidelitySnapshot{Policies: []Policy{{Name: "tenant_read", Command: "r", Roles: []string{"reader"}}}}
	have := FidelitySnapshot{Policies: []Policy{{Name: "tenant_read", Command: "r", Roles: []string{"reader", "auditor"}}}}

	drifted, err := fidelityDrift(have, want)
	require.NoError(t, err)
	assert.Equal(t, []string{"policies"}, drifted)
}
