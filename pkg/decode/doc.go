// Package decode owns the copy-and-swap route's logical-decoding state and
// the contracts its consumers depend on (ST-3, ST-4, CO-4, CO-8). It creates
// the single-table publication and the replication slot preflight named for
// a target, with an exported snapshot and the consistent point decoding
// starts from; drops them, waiting out a walsender that still holds the slot
// and never touching a slot another database owns; and reads a slot's
// pg_replication_slots row, including the WAL it retains and whether the
// server has declared that WAL lost. A Stream decodes the slot with pgoutput
// into ChangeEvents — one per committed row change, every column carried
// with its presence, so an unchanged out-of-line value the server omitted is
// never mistaken for a value, and the key a moved row had — and keeps two
// positions apart: what it has delivered, which nothing unyielded lies
// below, and what the caller has confirmed applied, which is the only
// position it ever reports to the server. Replication commands run on a
// dedicated connection from pkg/dbconn; catalog reads and the publication
// run on the caller's pool.
package decode
