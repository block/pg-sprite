package schemadiff_test

import (
	"encoding/json"
	"testing"

	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReviewCommentOnlyKeepsBeforeSnapshot(t *testing.T) {
	oldComment, newComment := "old explanation", "new explanation"
	live := schemadiff.Model{Table: "documents", RowSecurity: schemadiff.RowSecurity{Policies: []schemadiff.Policy{
		{Name: "readers", Command: schemadiff.PolicySelect, Permissive: true, Roles: []string{"public"}, Comment: &oldComment},
	}}}
	desired := live
	desired.RowSecurity.Policies = append([]schemadiff.Policy(nil), live.RowSecurity.Policies...)
	desired.RowSecurity.Policies[0].Comment = &newComment
	r, err := schemadiff.ReviewRowSecurity("public", live, desired)
	require.NoError(t, err)
	require.Len(t, r.Changes, 1)
	c := r.Changes[0]
	assert.Equal(t, schemadiff.AccessMetadataOnly, c.Impact)
	assert.Equal(t, "old explanation", *c.BeforePolicy.Comment)
	assert.Equal(t, "new explanation", *c.AfterPolicy.Comment)
	oldComment = "mutated"
	live.RowSecurity.Policies[0].Roles[0] = "mutated"
	assert.Equal(t, "old explanation", *c.BeforePolicy.Comment)
	assert.Equal(t, []string{"public"}, c.BeforePolicy.Roles)
}

func TestReviewRemovingForceMayWidenAccess(t *testing.T) {
	live := schemadiff.Model{Table: "documents", RowSecurity: schemadiff.RowSecurity{Enabled: true, Forced: true}}
	desired := live
	desired.RowSecurity.Forced = false
	r, err := schemadiff.ReviewRowSecurity("public", live, desired)
	require.NoError(t, err)
	require.Len(t, r.Changes, 1)
	assert.Equal(t, schemadiff.SecurityForced, r.Changes[0].Kind)
	assert.Equal(t, schemadiff.AccessMayWiden, r.Changes[0].Impact)
}

func TestReviewRestrictiveToPermissiveMayWidenAccess(t *testing.T) {
	live := schemadiff.Model{Table: "documents", RowSecurity: schemadiff.RowSecurity{Policies: []schemadiff.Policy{
		{Name: "readers", Command: schemadiff.PolicySelect, Roles: []string{"public"}},
	}}}
	desired := live
	desired.RowSecurity.Policies = append([]schemadiff.Policy(nil), live.RowSecurity.Policies...)
	desired.RowSecurity.Policies[0].Permissive = true
	r, err := schemadiff.ReviewRowSecurity("public", live, desired)
	require.NoError(t, err)
	require.Len(t, r.Changes, 1)
	assert.Equal(t, schemadiff.AccessMayWiden, r.Changes[0].Impact)
}

func TestReviewUnsupportedTableComparisonIsExplicit(t *testing.T) {
	live := schemadiff.Model{Table: "documents"}
	desired := live
	desired.Unlogged = true
	r, err := schemadiff.ReviewRowSecurity("public", live, desired)
	require.NoError(t, err)
	assert.False(t, r.TableComparisonComplete)
	assert.NotEmpty(t, r.TableComparisonError)
	assert.Empty(t, r.Changes)
}

func TestReviewAddedPolicyJSONHasNullBefore(t *testing.T) {
	change := schemadiff.SecurityChange{Kind: schemadiff.SecurityPolicyAdded,
		AfterPolicy: &schemadiff.PolicySnapshot{Name: "readers"}}
	raw, err := json.Marshal(change)
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &fields))
	require.Contains(t, fields, "before_policy")
	assert.JSONEq(t, "null", string(fields["before_policy"]))
	require.Contains(t, fields, "after_policy")
	var after schemadiff.PolicySnapshot
	require.NoError(t, json.Unmarshal(fields["after_policy"], &after))
	assert.Equal(t, "readers", after.Name)
}

func TestReviewRemovedPolicyJSONHasNullAfter(t *testing.T) {
	change := schemadiff.SecurityChange{Kind: schemadiff.SecurityPolicyRemoved,
		BeforePolicy: &schemadiff.PolicySnapshot{Name: "readers"}}
	raw, err := json.Marshal(change)
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &fields))
	require.Contains(t, fields, "after_policy")
	assert.JSONEq(t, "null", string(fields["after_policy"]))
	require.Contains(t, fields, "before_policy")
	var before schemadiff.PolicySnapshot
	require.NoError(t, json.Unmarshal(fields["before_policy"], &before))
	assert.Equal(t, "readers", before.Name)
}
