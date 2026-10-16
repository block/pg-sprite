package migrate

import (
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/refusal"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/verdict"
)

// This file is the copy-and-swap half of the front door's refusal registry
// (docs/refusal-classes.md): the two closed cause sets the route can refuse
// with — the preflight's shape and environment causes, and the shadow
// operations' fail-closed causes — each mapped onto one verdict refusal under
// ReasonCopyAndSwapRefused. The cause rides on the verdict unchanged, so an
// importer that already switches on the route's own vocabulary keeps doing
// so; the class is the route's class, converted token for token. Both
// mappings are total over their set and fail closed outside it, and
// TestRefusalRegistryIsComplete walks both sets.

// copySwapPreflightRefusal classifies a copy-and-swap preflight cause. The
// preflight declares no class of its own, so the classification is fixed
// here: a cause the operator clears by changing the table, the cluster, or
// the volume is environmental; a table shape the route does not carry yet
// is a capability boundary. ok is false for a cause outside
// preflight.CopySwapRefusalCauses().
func copySwapPreflightRefusal(cause preflight.CopySwapRefusalCause) (verdict.Refusal, bool) {
	switch cause {
	case preflight.CopySwapCauseReplicaIdentity,
		preflight.CopySwapCauseLogicalDecodingUnavailable,
		preflight.CopySwapCauseSlotCollision,
		preflight.CopySwapCauseSlotHeadroom,
		preflight.CopySwapCauseDiskHeadroom:
		return verdict.Environmental(verdict.ReasonCopyAndSwapRefused).WithCause(verdict.Cause(cause)), true
	case preflight.CopySwapCausePKUnsupported,
		preflight.CopySwapCauseForeignKeys,
		preflight.CopySwapCauseTriggers,
		preflight.CopySwapCausePartitioned,
		preflight.CopySwapCauseUnlogged,
		preflight.CopySwapCauseForceRLS,
		preflight.CopySwapCauseDependentViews,
		preflight.CopySwapCausePublicationMember,
		preflight.CopySwapCauseSubscriptionTarget,
		preflight.CopySwapCauseDependents:
		return verdict.CapabilityBoundary(verdict.ReasonCopyAndSwapRefused).WithCause(verdict.Cause(cause)), true
	default:
		return verdict.Refusal{}, false
	}
}

// shadowRefusal classifies a shadow operation's refusal cause by the class
// the cause declares for itself. ok is false for a cause outside
// schemachange.RefusalCauses(), which declares no class.
func shadowRefusal(cause schemachange.RefusalCause) (verdict.Refusal, bool) {
	r, ok := refusalOfClass(cause.Class(), verdict.ReasonCopyAndSwapRefused)
	if !ok {
		return verdict.Refusal{}, false
	}
	return r.WithCause(verdict.Cause(cause)), true
}

// refusalOfClass mints the verdict refusal for a core refusal class and a
// reason. The core's class vocabulary and the verdict contract's are kept
// apart so the core never imports the renderer; the tokens are the contract
// between them, and this is where one becomes the other. ok is false for
// the zero class and for a token the verdict contract does not name.
func refusalOfClass(class refusal.Class, reason verdict.Reason) (verdict.Refusal, bool) {
	switch class {
	case refusal.ClassCapabilityBoundary:
		return verdict.CapabilityBoundary(reason), true
	case refusal.ClassEnvironmental:
		return verdict.Environmental(reason), true
	case refusal.ClassInvariantViolation:
		return verdict.InvariantViolation(reason), true
	default:
		return verdict.Refusal{}, false
	}
}
