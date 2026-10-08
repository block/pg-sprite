// Package decode owns the copy-and-swap route's logical-decoding state and
// the contracts its consumers depend on (ST-3, ST-4, CO-4). It creates the
// single-table publication and the replication slot preflight named for a
// target, with an exported snapshot and the consistent point decoding starts
// from; drops them, waiting out a walsender that still holds the slot and
// never touching a slot another database owns or a publication it did not
// make; and reads a slot's pg_replication_slots row, including the WAL it
// retains and whether the server has declared that WAL lost. Replication
// commands run on a dedicated connection from pkg/dbconn, proven before any
// slot command to be a session of the same cluster and database as the
// caller's pool; catalog reads and the publication run on that pool.
// Refusals are typed — *ForeignStateError for state of the derived name
// that is not the route's, *PublicationPrivilegeError when the role may not
// create the publication, *SlotExistsError for the route's own earlier slot
// — so a caller routes on the type, never on message text.
package decode
