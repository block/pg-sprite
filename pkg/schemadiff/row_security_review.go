package schemadiff

import "slices"

// SecurityChangeKind names a review-only RLS delta, never executable SQL.
type SecurityChangeKind string

// Security change kinds are part of the versioned RLS review contract.
const (
	SecurityEnabled       SecurityChangeKind = "enabled"
	SecurityForced        SecurityChangeKind = "forced"
	SecurityPolicyAdded   SecurityChangeKind = "policy-added"
	SecurityPolicyRemoved SecurityChangeKind = "policy-removed"
	SecurityPolicyChanged SecurityChangeKind = "policy-changed"
)

// AccessImpact is advisory: it never proves effective access or predicate equivalence.
type AccessImpact string

// Known widening patterns are flagged even when other changes could offset them.
const (
	AccessMayWiden       AccessImpact = "may-widen"
	AccessReviewRequired AccessImpact = "review-required"
	AccessMetadataOnly   AccessImpact = "metadata-only"
)

// SecurityChange carries either before/after settings or complete policy snapshots.
// Missing policy sides are null for additions/removals; absent clauses stay null.
type SecurityChange struct {
	Kind          SecurityChangeKind `json:"kind"`
	Policy        string             `json:"policy,omitempty"`
	BeforeSetting *bool              `json:"before_setting,omitempty"`
	AfterSetting  *bool              `json:"after_setting,omitempty"`
	BeforePolicy  *PolicySnapshot    `json:"before_policy,omitempty"`
	AfterPolicy   *PolicySnapshot    `json:"after_policy,omitempty"`
	Impact        AccessImpact       `json:"access_impact"`
}

// RowSecurityReview is diagnostic only. It has no SQL or approval fingerprint.
// Version 1 compares captured catalog definitions, not grants or helper bodies.
type RowSecurityReview struct {
	Version                 int              `json:"version"`
	Changes                 []SecurityChange `json:"changes"`
	TableChanged            bool             `json:"table_changed"`
	TableComparisonComplete bool             `json:"table_comparison_complete"`
	TableComparisonError    string           `json:"table_comparison_error,omitempty"`
}

// ReviewRowSecurity describes RLS changes and reports whether ordinary table
// changes accompany them. An unsupported table comparison remains explicit.
func ReviewRowSecurity(schema string, live, desired Model) (RowSecurityReview, error) {
	if live.Table != desired.Table {
		return RowSecurityReview{}, ErrDifferentTables
	}
	r := RowSecurityReview{Version: 1, Changes: make([]SecurityChange, 0)}
	a, b := live.RowSecurity, desired.RowSecurity
	if a.Enabled != b.Enabled {
		r.Changes = append(r.Changes, settingChange(SecurityEnabled, a.Enabled, b.Enabled))
	}
	if a.Forced != b.Forced {
		r.Changes = append(r.Changes, settingChange(SecurityForced, a.Forced, b.Forced))
	}
	before, after := make(map[string]Policy), make(map[string]Policy)
	names := make([]string, 0)
	for _, p := range a.Policies {
		before[p.Name] = p
		names = append(names, p.Name)
	}
	for _, p := range b.Policies {
		after[p.Name] = p
		if _, ok := before[p.Name]; !ok {
			names = append(names, p.Name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		old, had := before[name]
		next, has := after[name]
		c := SecurityChange{Policy: name, Impact: AccessReviewRequired}
		switch {
		case !had:
			c.Kind = SecurityPolicyAdded
			c.AfterPolicy = policySnapshot(next)
			if next.Permissive {
				c.Impact = AccessMayWiden
			}
		case !has:
			c.Kind = SecurityPolicyRemoved
			c.BeforePolicy = policySnapshot(old)
			if !old.Permissive {
				c.Impact = AccessMayWiden
			}
		default:
			if policyEqual(old, next) {
				continue
			}
			c.Kind = SecurityPolicyChanged
			c.BeforePolicy = policySnapshot(old)
			c.AfterPolicy = policySnapshot(next)
			if !old.Permissive && next.Permissive {
				c.Impact = AccessMayWiden
			}
			comparison := old
			comparison.Comment = next.Comment
			if policyEqual(comparison, next) {
				c.Impact = AccessMetadataOnly
			}
		}
		r.Changes = append(r.Changes, c)
	}
	live.RowSecurity = RowSecurity{}
	desired.RowSecurity = RowSecurity{}
	changes, err := Diff(schema, live, desired)
	r.TableComparisonComplete = err == nil
	if err != nil {
		r.TableComparisonError = err.Error()
	} else {
		r.TableChanged = len(changes) > 0
	}
	return r, nil
}

func settingChange(kind SecurityChangeKind, before, after bool) SecurityChange {
	c := SecurityChange{Kind: kind, BeforeSetting: &before, AfterSetting: &after, Impact: AccessReviewRequired}
	if before && !after {
		c.Impact = AccessMayWiden
	}
	return c
}

// PolicySnapshot is the captured definition in the review JSON contract.
// Null expressions distinguish omitted clauses from explicit predicates.
type PolicySnapshot struct {
	Name       string        `json:"name"`
	Command    PolicyCommand `json:"command"`
	Permissive bool          `json:"permissive"`
	Roles      []string      `json:"roles"`
	Using      *string       `json:"using"`
	WithCheck  *string       `json:"with_check"`
	Comment    *string       `json:"comment"`
}

func policySnapshot(p Policy) *PolicySnapshot {
	return &PolicySnapshot{Name: p.Name, Command: p.Command, Permissive: p.Permissive,
		Roles: slices.Clone(p.Roles), Using: cloneText(p.Using), WithCheck: cloneText(p.WithCheck), Comment: cloneText(p.Comment)}
}

func cloneText(s *string) *string {
	if s == nil {
		return nil
	}
	copy := *s
	return &copy
}
