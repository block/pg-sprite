// Package schemachange orchestrates copy-and-swap schema changes while enforcing LK-2, LK-4, and ST-5.
//
// Every shadow operation takes the *dbconn.TableLockSession for the table:
// a dedicated server session, separate from the working pool, that holds
// the per-table lock (LK-1) for as long as the change runs.
//
// A build that finds a relation under the shadow's name returns
// ErrShadowExists. The resume order is: InspectShadow first, compare the
// returned BuiltShadow with the checkpoint the earlier build produced, and
// continue from that shadow when they agree; DropShadow only when they do
// not, or when the change is being abandoned. Inspection costs one read
// transaction; a drop destroys a shadow that may have been resumable and
// buys a full rebuild and copy.
//
// Runner chains the operations for a quiesced table — one no writer
// touches while the change runs — from table lock to old table drop,
// writing the checkpoint row at every phase transition and dispatching on
// the row it finds, so a run that stops anywhere resumes from the row.
package schemachange
