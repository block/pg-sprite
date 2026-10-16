// Package refusal holds the routing vocabulary the core's refusals are
// classified in. A refusal cause says what the engine found; its class says
// what the consumer does about it — retry or clean up, wait for a
// capability, or report a bug — and an importer routes on the class without
// reading the cause's text. The classes are decided in
// docs/refusal-classes.md; this package carries the ones a core refusal can
// take, so a cause names its class where it is declared and the periphery
// maps it onto the verdict contract without the core importing a renderer.
package refusal

// Class is the routing category of a refusal: what the consumer does about
// it.
type Class string

const (
	// ClassCapabilityBoundary means the engine has no implemented safe route
	// for the form it was handed; the consumer waits for the capability or
	// changes the statement.
	ClassCapabilityBoundary Class = "capability-boundary"
	// ClassEnvironmental means the operation is supportable but a lock, a
	// proof, or a relation moved under it; the consumer re-acquires,
	// re-plans, or cleans up and repeats.
	ClassEnvironmental Class = "environmental"
	// ClassInvariantViolation means the engine was handed an incoherent
	// input or its own write did not take; the consumer reports it and does
	// not retry, since nothing on the server changes the outcome.
	ClassInvariantViolation Class = "invariant-violation"
)

// Classes returns the closed set of classes a core refusal can take.
func Classes() []Class {
	return []Class{ClassCapabilityBoundary, ClassEnvironmental, ClassInvariantViolation}
}
