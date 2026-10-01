package schemachange

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// fidelityDrift names the facts of have that differ from want, in snapshot
// field order, so a refusal can say what moved rather than that something
// did. Each fact is compared as its canonical JSON, the same encoding the
// checkpoint stores, so the two paths agree on what "equal" means.
func fidelityDrift(have, want FidelitySnapshot) ([]string, error) {
	facts := []struct {
		name       string
		have, want any
	}{
		{"owner", have.Owner, want.Owner},
		{"replica_identity", have.ReplicaIdentity, want.ReplicaIdentity},
		{"rls_enabled", have.RLSEnabled, want.RLSEnabled},
		{"rls_forced", have.RLSForced, want.RLSForced},
		{"comment", have.Comment, want.Comment},
		{"tablespace", have.Tablespace, want.Tablespace},
		{"rel_options", have.RelOptions, want.RelOptions},
		{"grants", have.Grants, want.Grants},
		{"column_grants", have.ColumnGrants, want.ColumnGrants},
		{"policies", have.Policies, want.Policies},
		{"unvalidated_checks", have.UnvalidatedChecks, want.UnvalidatedChecks},
		{"column_statistics_targets", have.ColumnStatisticsTargets, want.ColumnStatisticsTargets},
	}
	var drifted []string
	for _, f := range facts {
		same, err := jsonEqual(f.have, f.want)
		if err != nil {
			return nil, fmt.Errorf("compare %s: %w", f.name, err)
		}
		if !same {
			drifted = append(drifted, f.name)
		}
	}
	return drifted, nil
}

// jsonEqual reports whether two values share one JSON encoding. A nil
// slice and an empty one encode differently; both sides are read by the
// same catalog reader, which yields the same shape for the same catalog.
func jsonEqual(a, b any) (bool, error) {
	encodedA, err := json.Marshal(a)
	if err != nil {
		return false, err
	}
	encodedB, err := json.Marshal(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(encodedA, encodedB), nil
}
