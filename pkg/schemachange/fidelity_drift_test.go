package schemachange

import (
	"reflect"
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

// Every fact the snapshot records is a named, comparable fact: a snapshot
// that differs from another in every field names every field, each by the
// JSON name the checkpoint stores it under. A field added to the snapshot
// without a JSON name, or one the comparison skipped, would let a difference
// pass the gate unnamed.
func TestFidelityDriftCoversEveryFactOfTheSnapshot(t *testing.T) {
	want := FidelitySnapshot{}
	have := FidelitySnapshot{
		Owner:                     "app",
		ReplicaIdentity:           "f",
		RLSEnabled:                true,
		RLSForced:                 true,
		Comment:                   "orders",
		Tablespace:                "fast",
		RelOptions:                []string{"fillfactor=70"},
		Grants:                    []Grant{{Privilege: "SELECT", Grantee: "reader"}},
		ColumnGrants:              []ColumnGrant{{Column: "note", Grant: Grant{Privilege: "SELECT", Grantee: "reader"}}},
		Policies:                  []Policy{{Name: "tenant_read", Command: "r"}},
		UnvalidatedChecks:         []UnvalidatedConstraint{{Name: "qty_positive", Def: "CHECK ((qty > 0)) NOT VALID"}},
		ColumnStatisticsTargets:   []ColumnStatisticsTarget{{Column: "tenant", Target: 500}},
		ExtendedStatisticsTargets: []ExtendedStatisticsTarget{{Name: "orders_tenant_qty_stat", Target: 250}},
	}
	fields := reflect.VisibleFields(reflect.TypeOf(have))
	for _, field := range fields {
		assert.False(t, reflect.ValueOf(have).FieldByIndex(field.Index).IsZero(), "the test sets %s so that it differs", field.Name)
	}

	drifted, err := fidelityDrift(have, want)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"owner", "replica_identity", "rls_enabled", "rls_forced", "comment", "tablespace",
		"rel_options", "grants", "column_grants", "policies", "unvalidated_checks",
		"column_statistics_targets", "extended_statistics_targets",
	}, drifted)
	assert.Len(t, drifted, len(fields), "every field of the snapshot is a named fact")
}
