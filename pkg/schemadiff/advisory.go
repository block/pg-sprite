package schemadiff

// AdvisoryKind classifies an observation the diff made but could not turn
// into a change, so a consumer can act on it without parsing prose.
type AdvisoryKind string

// The advisory kinds a plan can carry. Both describe an invalid index on a
// plain table — an unfinished concurrent build, abandoned or still running
// — which the diff never drops (see indexNeedsDrop).
const (
	// AdvisoryInvalidIndexRebuild marks an invalid index the desired
	// schema names, with the same or a different definition: the plan
	// carries its create-index, and that create cannot run as-is against
	// the occupied name. Completing the plan goes through the proven
	// removal path (executor.RebuildAbandonedIndex or the invalid-index
	// runbook), which renames and drops the entry only after proving it
	// abandoned.
	AdvisoryInvalidIndexRebuild AdvisoryKind = "invalid-index-rebuild"
	// AdvisoryInvalidIndexUndeclared marks an invalid index the desired
	// schema does not name: the diff plans no drop for it, so no statement
	// touches it and an empty plan is not evidence the table is clean. An
	// abandoned entry clears per the invalid-index runbook; a
	// build still running finishes on its own.
	AdvisoryInvalidIndexUndeclared AdvisoryKind = "invalid-index-undeclared"
)

// AdvisoryKinds returns the closed set of AdvisoryKind values. It is part
// of the plan-report contract (docs/plan-report.md): the set changes only
// with a format_version bump, and a consumer that meets an unrecognized
// value must treat the advisory as unknown and surface it.
func AdvisoryKinds() []AdvisoryKind {
	return []AdvisoryKind{
		AdvisoryInvalidIndexRebuild,
		AdvisoryInvalidIndexUndeclared,
	}
}

// Description renders the kind for an operator: what the diff observed and
// what completes the plan. Display only — consumers branch on the kind.
func (k AdvisoryKind) Description() string {
	switch k {
	case AdvisoryInvalidIndexRebuild:
		return "is an unfinished concurrent build (invalid) that the desired schema names; the planned create cannot run against the occupied name, and the plan completes only through the proven-removal path (executor.RebuildAbandonedIndex or the invalid-index runbook), which drops the entry after proving it abandoned"
	case AdvisoryInvalidIndexUndeclared:
		return "is an unfinished concurrent build (invalid) that the desired schema does not name; the diff never drops it, so no planned statement touches it and an empty plan does not mean the table is clean — clear an abandoned entry per the invalid-index runbook, or wait for a build still running to finish"
	}
	return string(k)
}

// Advisory is one typed observation the diff made about the live table
// that produced no change: reported so a consumer sees what the plan
// declined to do, never acted on by the engine. The table is the report's.
type Advisory struct {
	// Kind classifies the observation.
	Kind AdvisoryKind `json:"kind"`
	// Index is the live index the observation is about.
	Index string `json:"index"`
}

// Advisories reports every invalid index on a plain live table, in live
// index order, classified by whether desired names it. These are the
// entries Diff observes and declines to drop; Diff's change list alone
// would discard the observation, so a caller reporting a plan pairs the two
// over the same models. A partitioned parent's invalid entries are not
// unfinished builds and produce no advisory.
func Advisories(live, desired Model) []Advisory {
	partitioned := live.PartitionKey != ""
	desiredIdx := indexesByName(desired.Indexes)
	var advisories []Advisory
	for _, ix := range live.Indexes {
		if !isUnfinishedBuild(partitioned, ix) {
			continue
		}
		kind := AdvisoryInvalidIndexUndeclared
		if _, named := desiredIdx[ix.Name]; named {
			kind = AdvisoryInvalidIndexRebuild
		}
		advisories = append(advisories, Advisory{Kind: kind, Index: ix.Name})
	}
	return advisories
}
