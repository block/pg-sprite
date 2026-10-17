// Package checkpoint persists the one durable resume record per target —
// the Checkpoint — in the target database, enforcing ST-1 (one row per
// target, written atomically), ST-2 (an incompatible record is
// distinguishable from a transient read error, and neither is mistaken for
// the absence of a record), and ST-10 (the table evolves additively, and an
// older engine never alters a newer one's table). The table's shape is
// derived from its columns: Ensure adds what an older engine's table lacks
// and refuses, untouched, a table a newer engine shaped (ErrTableNewer); a
// read or write that reaches an older table before Ensure ran reports
// ErrTableBehind. The row's format is a separate question from the table's
// shape: a row an engine of another FormatVersion wrote surfaces from
// Inspect and Load as an *IncompatibleError on MismatchFormat carrying the
// row's Identity, which Delete takes for the explicit fresh start. The
// phase set grows within a format without a bump, so a row a newer engine
// of the same format left in a phase this one does not know surfaces the
// same way, on MismatchPhase.
package checkpoint
