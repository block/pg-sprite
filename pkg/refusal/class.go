// Package refusal holds the routing vocabulary the core's refusals are
// classified in. A refusal cause says what the engine found; its class says
// what kind of boundary was reached — a capability the engine lacks, a form
// it declines for good, state outside the engine that moved, or an
// incoherence in its own input or writes — and an importer triages on the
// class without reading the cause's text. The classes are decided in
// docs/refusal-classes.md; this package carries the ones the cause tables
// there use, so a cause names its class where it is declared and the
// periphery maps it onto the verdict contract without the core importing a
// renderer. The verdict contract's one further class,
// no-online-safety-problem, describes a statement that never reached the
// engine and no engine cause takes it.
//
// A class is a triage decision, not an action: the row for the cause in
// docs/refusal-classes.md decides what the consumer does, and a class on its
// own never licenses an automatic retry.
package refusal

import "slices"

// Class is the routing category of a refusal: what kind of boundary the
// consumer is looking at. The empty Class means the error is not a typed
// refusal of the engine; the consumer routes it by SQLSTATE or by sentinel
// instead, as docs/refusal-classes.md describes for a server error.
type Class string

const (
	// ClassCapabilityBoundary means the engine has no implemented safe route
	// for the form it was handed; the consumer waits for the capability or
	// changes the statement.
	ClassCapabilityBoundary Class = "capability-boundary"
	// ClassByDesign means the engine refuses the form for good, because the
	// server offers no online mechanism for it or a safer idiom exists; the
	// consumer takes the idiom or the deliberate path the cause names, and
	// waiting for a release waits for nothing.
	ClassByDesign Class = "by-design"
	// ClassEnvironmental means the operation is supportable but was stopped
	// by state outside the engine — a lock, a proof, a relation, a slot, or
	// a setting that moved under it or stands in its way. The consumer
	// re-acquires, re-plans, cleans up, or hands the catalog to an operator,
	// as the cause's row says; some environmental causes forbid any retry
	// until an operator has read the catalog, so the class alone decides
	// nothing about retrying.
	ClassEnvironmental Class = "environmental"
	// ClassInvariantViolation means the engine was handed an incoherent
	// input or its own write did not take; the consumer reports it and does
	// not retry, since nothing on the server changes the outcome.
	ClassInvariantViolation Class = "invariant-violation"
)

// Classes returns the closed set of classes the engine's typed refusal
// causes take: every class cell of every cause table in
// docs/refusal-classes.md is one of these.
func Classes() []Class {
	return []Class{ClassCapabilityBoundary, ClassByDesign, ClassEnvironmental, ClassInvariantViolation}
}

// Valid reports whether c is one of Classes. The empty Class is not valid,
// since it marks an error that is not a typed refusal; a Class read back
// from stored output is validated with this before anything routes on it.
func (c Class) Valid() bool {
	return slices.Contains(Classes(), c)
}
