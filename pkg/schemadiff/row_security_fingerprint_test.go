package schemadiff

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRowSecurityFingerprintBindsCompleteDefinitions(t *testing.T) {
	live := Model{Table: "documents", Columns: []Column{{Name: "id", Type: "bigint"}}, RowSecurity: RowSecurity{Enabled: true}}
	desired := live
	desired.RowSecurity.Enabled = false
	original, err := ReviewRowSecurity("public", live, desired)
	require.NoError(t, err)
	again, err := ReviewRowSecurity("public", live, desired)
	require.NoError(t, err)
	require.NoError(t, again.CheckFingerprint(original.Fingerprint))
	assert.Equal(t, 2, original.Version)
	for _, change := range []struct {
		name   string
		mutate func(*Model)
	}{
		{"force", func(m *Model) { m.RowSecurity.Forced = true }},
		{"policy", func(m *Model) { m.RowSecurity.Policies = []Policy{{Name: "readers", Roles: []string{"public"}}} }},
		{"table", func(m *Model) { m.Columns = []Column{{Name: "id", Type: "integer"}} }},
	} {
		t.Run(change.name, func(t *testing.T) {
			for _, side := range []string{"live", "desired", "both"} {
				t.Run(side, func(t *testing.T) {
					a, b := live, desired
					if side != "desired" {
						change.mutate(&a)
					}
					if side != "live" {
						change.mutate(&b)
					}
					current, err := ReviewRowSecurity("public", a, b)
					require.NoError(t, err)
					err = current.CheckFingerprint(original.Fingerprint)
					require.ErrorIs(t, err, ErrStaleRowSecurityReview)
					var stale *RowSecurityReviewStale
					require.ErrorAs(t, err, &stale)
					assert.Equal(t, original.Fingerprint, stale.Expected)
					assert.Equal(t, current.Fingerprint, stale.Actual)
				})
			}
		})
	}
	other, err := ReviewRowSecurity("other", live, desired)
	require.NoError(t, err)
	assert.ErrorIs(t, other.CheckFingerprint(original.Fingerprint), ErrStaleRowSecurityReview)
	live.Table, desired.Table = "other", "other"
	other, err = ReviewRowSecurity("public", live, desired)
	require.NoError(t, err)
	assert.ErrorIs(t, other.CheckFingerprint(original.Fingerprint), ErrStaleRowSecurityReview)
}

func TestRowSecurityFingerprintIncludesEveryPolicyField(t *testing.T) {
	predicate, comment := "true", "readers"
	baseline := Policy{Name: "readers", Command: PolicySelect, Permissive: true, Roles: []string{"public"}, Using: &predicate}
	live := Model{Table: "documents", RowSecurity: RowSecurity{Policies: []Policy{baseline}}}
	original, err := ReviewRowSecurity("public", live, live)
	require.NoError(t, err)
	for _, change := range []struct {
		name   string
		mutate func(*Policy)
	}{
		{"name", func(p *Policy) { p.Name = "different" }},
		{"command", func(p *Policy) { p.Command = PolicyAll }},
		{"mode", func(p *Policy) { p.Permissive = false }},
		{"roles", func(p *Policy) { p.Roles = []string{"reader"} }},
		{"using", func(p *Policy) { p.Using = nil }},
		{"with check", func(p *Policy) { p.WithCheck = &predicate }},
		{"comment", func(p *Policy) { p.Comment = &comment }},
	} {
		t.Run(change.name, func(t *testing.T) {
			policy := baseline
			change.mutate(&policy)
			changed := live
			changed.RowSecurity.Policies = []Policy{policy}
			review, err := ReviewRowSecurity("public", changed, changed)
			require.NoError(t, err)
			assert.Empty(t, review.Changes, "even an unchanged policy belongs to the captured state")
			assert.ErrorIs(t, review.CheckFingerprint(original.Fingerprint), ErrStaleRowSecurityReview)
		})
	}
}

func TestRowSecurityFingerprintRejectsInvalidIdentities(t *testing.T) {
	for _, value := range []string{"", "abc", "rls-review-v2:" + strings.Repeat("a", 64), "rls-review-v1:" + strings.Repeat("G", 64), "rls-review-v1:" + strings.Repeat("A", 64)} {
		assert.ErrorIs(t, ValidateRowSecurityFingerprint(value), ErrInvalidRowSecurityFingerprint)
	}
	assert.ErrorIs(t, (RowSecurityReview{}).CheckFingerprint("rls-review-v1:"+strings.Repeat("a", 64)), ErrInvalidRowSecurityFingerprint)
}

// Pin the encoding so adding a Model field cannot silently change identities
// without an intentional fingerprint-version decision.
func TestRowSecurityFingerprintEncoding(t *testing.T) {
	model := Model{Table: "documents"}
	review, err := ReviewRowSecurity("public", model, model)
	require.NoError(t, err)
	assert.Equal(t, "rls-review-v1:808c50bfc703af0ac9cfae06bc062e8405cd7205cb56890bb581394974fd078e", review.Fingerprint)
}
