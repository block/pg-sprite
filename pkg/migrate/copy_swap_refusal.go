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

// copySwapPreflightRefusal classifies a copy-and-swap preflight cause by the
// class the cause declares for itself (docs/refusal-classes.md). ok is false
// for a cause outside preflight.CopySwapRefusalCauses(), which declares no
// class.
func copySwapPreflightRefusal(cause preflight.CopySwapRefusalCause) (verdict.Refusal, bool) {
	r, ok := refusalOfClass(cause.Class())
	if !ok {
		return verdict.Refusal{}, false
	}
	return r.WithCause(verdict.Cause(cause)), true
}

// shadowRefusal classifies a shadow operation's refusal cause by the class
// the cause declares for itself. ok is false for a cause outside
// schemachange.RefusalCauses(), which declares no class.
func shadowRefusal(cause schemachange.RefusalCause) (verdict.Refusal, bool) {
	r, ok := refusalOfClass(cause.Class())
	if !ok {
		return verdict.Refusal{}, false
	}
	return r.WithCause(verdict.Cause(cause)), true
}

// refusalOfClass mints the copy-and-swap verdict refusal for a core refusal
// class. The core's class vocabulary and the verdict contract's are kept
// apart so the core never imports the renderer; the tokens are the contract
// between them, and this is where one becomes the other. ok is false for
// the zero class and for a token the verdict contract does not name.
func refusalOfClass(class refusal.Class) (verdict.Refusal, bool) {
	switch class {
	case refusal.ClassCapabilityBoundary:
		return verdict.CapabilityBoundary(verdict.ReasonCopyAndSwapRefused), true
	case refusal.ClassByDesign:
		return verdict.ByDesign(verdict.ReasonCopyAndSwapRefused), true
	case refusal.ClassEnvironmental:
		return verdict.Environmental(verdict.ReasonCopyAndSwapRefused), true
	case refusal.ClassInvariantViolation:
		return verdict.InvariantViolation(verdict.ReasonCopyAndSwapRefused), true
	default:
		return verdict.Refusal{}, false
	}
}
