// Package checkpoint persists the one durable resume record per target —
// the Checkpoint — in the target database, enforcing ST-1 (one row per
// target, written atomically) and ST-2 (an incompatible record is
// distinguishable from a transient read error, and neither is mistaken for
// the absence of a record). The table's shape is derived from its columns:
// Ensure adds what an older engine's table lacks and refuses, untouched, a
// table a newer engine shaped.
package checkpoint
