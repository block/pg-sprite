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
// Runner chains the operations from table lock to old table drop, writing
// the checkpoint row at every phase transition and dispatching on the row
// it finds, so a run that stops anywhere resumes from the row. The target's
// proof chooses the route. A quiesced table's quiescence is enforced, not
// assumed: the run fences the source with a statement trigger that refuses
// every write until the swap, and the fence stays on a stopped run's
// source until it is resumed or abandoned with DropWriteFence. A table
// under writes is captured instead: the run creates the slot with the row
// that owns it, applies the stream to the shadow beside the copy, catches
// up to a marker past every change committed so far, verifies live, and
// drains the stream under the swap's lock before it renames; a stopped
// run's slot stays until it is resumed or abandoned with decode.DropSlot.
// The one exception to "a shadow is only dropped on abandonment" is a
// first save whose row does not land: the run drops the shadow it just
// built, and the slot with it, since a shadow no row records would refuse
// the next run as ErrShadowExists.
package schemachange
