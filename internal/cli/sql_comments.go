package cli

import "strings"

// PostgreSQL ends a line comment at either CR or LF. Normalize both before
// prefixing every line, including untrusted identifiers and diagnostic text.
func sqlDiagnosticComment(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return "-- " + strings.ReplaceAll(text, "\n", "\n-- ")
}
