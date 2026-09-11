package verdict

// AcceptedBlockingEligible reports whether a typed refusal is in the closed
// accepted-blocking registry. Unknown combinations fail closed.
func AcceptedBlockingEligible(r Refusal) bool {
	eligible, decided := AcceptedBlockingDecision(r)
	return decided && eligible
}

// AcceptedBlockingDecision reports eligibility and whether the refusal key
// has an explicit decision. Completeness tests use decided to reject additions
// to the refusal vocabulary that have not been classified here.
func AcceptedBlockingDecision(r Refusal) (eligible, decided bool) {
	switch r.Reason() {
	case ReasonIndexStatement:
		switch r.Site() {
		case RefusalSiteIndexSingleRelation:
			return r.Class() == ClassByDesign, true
		case RefusalSiteIndexOther, "":
			return false, true
		default:
			return false, false
		}
	case ReasonUnsupportedPartitionedParent:
		switch r.Cause() {
		case CauseParentBlockingIndexBuild:
			return r.Class() == ClassCapabilityBoundary, true
		case CauseParentConcurrentIndexBuild, CauseParentIndexAdoption, CauseParentNotValidForeignKey:
			return false, true
		default:
			return false, false
		}
	case ReasonUnsupportedStatement, ReasonTableTooLarge, ReasonInsufficientPrivileges,
		ReasonBudgetExceeded, ReasonRewriteRequired, ReasonBackendUnavailable,
		ReasonDestructiveChange, ReasonPlanFingerprintMismatch, ReasonCreateCollision:
		return false, true
	default:
		return false, false
	}
}
