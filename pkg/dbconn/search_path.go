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

// unshadowCatalog runs once per new physical connection and removes any
// explicit pg_catalog entry from the session's search_path.
//
// PostgreSQL searches pg_catalog implicitly before every search_path entry
// unless the path names it explicitly — then it is searched at that
// position, and a user schema listed before it shadows catalog names. A role
// or database configured as `search_path = app, pg_catalog` would make an
// unqualified `pg_class` resolve to `app.pg_class` on every pooled session,
// so a decoy table could turn a preflight read into a wrong answer. Dropping
// the explicit entry restores the implicit-first rule without touching the
// rest of the path: every other entry keeps its position, and the creation
// schema (current_schema()) is exactly what the caller configured, so
// unqualified statements resolve as they always did.
func unshadowCatalog(ctx context.Context, conn *pgx.Conn) error {
	var path string
	if err := conn.QueryRow(ctx, "SHOW search_path").Scan(&path); err != nil {
		return fmt.Errorf("read search_path: %w", err)
	}
	unshadowed, changed := withoutExplicitCatalog(path)
	if !changed {
		return nil
	}
	if _, err := conn.Exec(ctx, "SELECT pg_catalog.set_config('search_path', $1, false)", unshadowed); err != nil {
		return fmt.Errorf("remove explicit %s from search_path: %w", catalogSchema, err)
	}
	return nil
}

// withoutExplicitCatalog returns path with every entry naming pg_catalog
// removed and reports whether anything was removed. The path is the raw
// setting as SHOW search_path prints it: comma-separated entries, each
// either a bare identifier (case-folded by the server, so compared
// case-insensitively) or a double-quoted identifier (compared exactly, with
// "" as the escaped quote). Every other entry is kept verbatim in its
// position; when nothing is removed the input is returned unchanged.
func withoutExplicitCatalog(path string) (string, bool) {
	entries := splitSearchPath(path)
	kept := make([]string, 0, len(entries))
	removed := false
	for _, entry := range entries {
		if namesCatalog(entry) {
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
func namesCatalog(entry string) bool {
	if strings.HasPrefix(entry, `"`) && strings.HasSuffix(entry, `"`) && len(entry) >= 2 {
		unquoted := strings.ReplaceAll(entry[1:len(entry)-1], `""`, `"`)
		return unquoted == catalogSchema
	}
	return strings.EqualFold(entry, catalogSchema)
}
