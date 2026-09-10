package dbconn

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// catalogSchema is the system schema that holds pg_class, pg_namespace and
// every other catalog relation and operator pg-sprite reads unqualified.
const catalogSchema = "pg_catalog"

// unshadowCatalog runs once per new physical connection and removes every
// pg_catalog entry that another schema precedes in the session's
// search_path.
//
// PostgreSQL searches pg_catalog implicitly before every search_path entry
// unless the path names it explicitly — then it is searched at that
// position, and a user schema listed before it shadows catalog names. A role
// or database configured as `search_path = app, pg_catalog` would make an
// unqualified `pg_class` resolve to `app.pg_class` on every pooled session,
// so a decoy table could turn a preflight read into a wrong answer. Dropping
// the shadowed entry restores the implicit-first rule without touching the
// rest of the path: every other entry keeps its position. A pg_catalog entry
// that leads the path shadows nothing and is kept, so `pg_catalog, public`
// — a common hardening — is left exactly as configured, creation schema
// included. The creation schema (current_schema()) changes only when every
// entry ahead of a removed pg_catalog names a schema that does not exist:
// it was pg_catalog, where unqualified CREATE is refused anyway, and
// becomes the next existing entry after the removed one, or NULL when there
// is none — an unqualified CREATE then fails with "no schema has been
// selected", which pg-sprite's preflight reports as ErrNoCreationSchema.
//
// Only an explicit pg_catalog entry is rewritten. The implicit pg_temp
// search that precedes it is untouched: pg-sprite creates no temporary
// objects, so nothing it runs can populate a pg_temp schema that would
// shadow the catalog for its own session. The write is a runtime set_config
// on the server connection, so under a transaction-pooling proxy the
// rewritten path may outlive the client that triggered it; the value it
// leaves behind is the stricter one.
//
// INV: CO-9 — this hook upholds the invariant for every session-path read;
// LocalSearchPath upholds it for every transaction-local path.
func unshadowCatalog(ctx context.Context, conn *pgx.Conn) error {
	var path string
	if err := conn.QueryRow(ctx, "SHOW search_path").Scan(&path); err != nil {
		return fmt.Errorf("read search_path: %w", err)
	}
	unshadowed, changed := withoutShadowedCatalog(path)
	if !changed {
		return nil
	}
	if _, err := conn.Exec(ctx, "SELECT pg_catalog.set_config('search_path', $1, false)", unshadowed); err != nil {
		return fmt.Errorf("remove shadowed %s from search_path: %w", catalogSchema, err)
	}
	return nil
}

// LocalSearchPath returns the SET LOCAL statement that scopes the current
// transaction's search_path to schemas, in order. SET cannot take bind
// parameters, so each schema is quoted as an identifier, and the path then
// receives the same rewrite as every pooled session's: a pg_catalog entry
// that another schema precedes is dropped. A transaction-local path replaces
// the session path for the transaction, so it must uphold the same
// guarantee (INV: CO-9); every transaction-local search_path pg-sprite sets
// is built here, and TestLocalSearchPathIsTheOnlySearchPathWriter keeps it
// that way.
func LocalSearchPath(schemas ...string) string {
	quoted := make([]string, len(schemas))
	for i, schema := range schemas {
		quoted[i] = pgx.Identifier{schema}.Sanitize()
	}
	path, _ := withoutShadowedCatalog(strings.Join(quoted, ", "))
	return "SET LOCAL search_path = " + path
}

// withoutShadowedCatalog returns path with every pg_catalog entry that a
// non-catalog entry precedes removed, and reports whether anything was
// removed. Leading pg_catalog entries are kept: nothing is ahead of them to
// shadow the catalog. The path is the raw setting as SHOW search_path prints
// it: comma-separated entries, each either a bare identifier (case-folded by
// the server, so compared case-insensitively) or a double-quoted identifier
// (compared exactly, with "" as the escaped quote). Every other entry is
// kept verbatim in its position; when nothing is removed the input is
// returned unchanged.
func withoutShadowedCatalog(path string) (string, bool) {
	entries := splitSearchPath(path)
	kept := make([]string, 0, len(entries))
	removed := false
	shadowed := false
	for _, entry := range entries {
		if !namesCatalog(entry) {
			shadowed = true
			kept = append(kept, entry)
			continue
		}
		if shadowed {
			removed = true
			continue
		}
		kept = append(kept, entry)
	}
	if !removed {
		return path, false
	}
	return strings.Join(kept, ", "), true
}

// splitSearchPath splits a search_path setting on the commas that sit
// outside double quotes and trims the whitespace around each entry. Empty
// entries are dropped, so an empty path yields no entries.
func splitSearchPath(path string) []string {
	var entries []string
	var current strings.Builder
	inQuotes := false
	flush := func() {
		entry := strings.TrimSpace(current.String())
		current.Reset()
		if entry != "" {
			entries = append(entries, entry)
		}
	}
	for _, r := range path {
		switch {
		case r == '"':
			inQuotes = !inQuotes
			current.WriteRune(r)
		case r == ',' && !inQuotes:
			flush()
		default:
			current.WriteRune(r)
		}
	}
	flush()
	return entries
}

// namesCatalog reports whether one search_path entry resolves to pg_catalog.
// A quoted entry is unquoted the general way — the surrounding quotes drop
// and "" becomes " — even though pg_catalog itself contains no quote, so
// the comparison stays correct if the target ever changes. The length guard
// keeps a lone quote, which the server never emits, from being unquoted as
// an empty name.
func namesCatalog(entry string) bool {
	if strings.HasPrefix(entry, `"`) && strings.HasSuffix(entry, `"`) && len(entry) >= 2 {
		unquoted := strings.ReplaceAll(entry[1:len(entry)-1], `""`, `"`)
		return unquoted == catalogSchema
	}
	return strings.EqualFold(entry, catalogSchema)
}
