// Package change is a change source with the method set of Spirit's
// change.Source, so a consumer written against that interface can be driven
// from PostgreSQL logical decoding. The interface and the types its methods
// take are declared here rather than imported (see source.go): pg-sprite
// does not depend on Spirit as a module, so the declarations are a copy that
// is kept in step by hand.
//
// LogicalSource implements it over pkg/decode. It streams the one table a
// copy-and-swap target proves, through the target's own slot and
// publication, and pushes every committed row change to the registered
// subscription as a key and a whole row image in the table's column order.
// The position it reports is a WAL position every change below which the
// subscription has flushed, and only that position is confirmed to the slot
// (ST-4), so a stream resumed from the slot replays at most what was never
// flushed. An UPDATE that left an out-of-line value unchanged carries no
// value for it (CO-8); a whole image cannot be built from it, so the source
// stops rather than hand a subscription a guess.
package change
