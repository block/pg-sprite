package schemadiff

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Only the text of a policy's expressions may differ for the live policies
// to be compared again on the file's table definition; any other
// difference in the row security is a difference on its own.
func TestPoliciesDifferOnlyInExpressions(t *testing.T) {
	text := func(s string) *string { return &s }
	policy := func() Policy {
		return Policy{Name: "owners", Command: PolicyAll, Permissive: true, Roles: []string{"public"}, Using: text("(owner_name = CURRENT_USER)")}
	}
	live := RowSecurity{Enabled: true, Policies: []Policy{policy()}}
	for name, tc := range map[string]struct {
		change func(*RowSecurity)
		want   bool
	}{
		"the expression text": {change: func(r *RowSecurity) { r.Policies[0].Using = text("((owner_name)::text = (CURRENT_USER)::text)") }, want: true},
		"nothing":             {change: func(*RowSecurity) {}, want: true},
		"a WITH CHECK added":  {change: func(r *RowSecurity) { r.Policies[0].WithCheck = text("true") }},
		"the roles":           {change: func(r *RowSecurity) { r.Policies[0].Roles = []string{"app"} }},
		"the command":         {change: func(r *RowSecurity) { r.Policies[0].Command = PolicySelect }},
		"the mode":            {change: func(r *RowSecurity) { r.Policies[0].Permissive = false }},
		"the name":            {change: func(r *RowSecurity) { r.Policies[0].Name = "readers" }},
		"the comment":         {change: func(r *RowSecurity) { r.Policies[0].Comment = text("who reads") }},
		"a policy added":      {change: func(r *RowSecurity) { r.Policies = append(r.Policies, policy()) }},
		"forced":              {change: func(r *RowSecurity) { r.Forced = true }},
		"disabled":            {change: func(r *RowSecurity) { r.Enabled = false }},
	} {
		desired := RowSecurity{Enabled: live.Enabled, Forced: live.Forced, Policies: []Policy{policy()}}
		tc.change(&desired)
		assert.Equal(t, tc.want, policiesDifferOnlyInExpressions(live, desired), name)
	}
}
