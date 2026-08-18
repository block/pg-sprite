package cli

import (
	"io"
	"os"
)

// OutputFlags are the presentation flags shared by every command that
// renders a human-facing diagnostic report. Color never touches the
// machine contracts: the JSON reports and the --sql script stay plain.
type OutputFlags struct {
	Color string `help:"Color the diagnostic report: auto colors only when stdout is a terminal (NO_COLOR or TERM=dumb disables it); always and never override the detection." enum:"auto,always,never" default:"auto"`
}

// palette resolves the color decision once for one report written to out,
// so every label in the report agrees.
func (f OutputFlags) palette(out io.Writer) palette {
	return palette{enabled: colorEnabled(f.Color, out)}
}

// colorEnabled decides whether ANSI color is emitted. An explicit
// --color=always or --color=never wins; auto colors only a terminal and
// respects the NO_COLOR convention (https://no-color.org) and TERM=dumb.
func colorEnabled(mode string, out io.Writer) bool {
	switch mode {
	case "always":
		return true
	case "never":
		return false
	}
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	return isTerminal(out)
}

// isTerminal reports whether out is a character device. Hand-rolled
// (fstat mode bit) rather than imported, per the little-copying maxim; an
// injected non-file writer (tests, buffers) is never a terminal.
func isTerminal(out io.Writer) bool {
	f, ok := out.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// ANSI SGR sequences for the diagnostic grammar, matching conventional
// compiler-diagnostic colors: errors red, warnings yellow, notes cyan,
// help green, structural labels bold.
const (
	ansiReset   = "\x1b[0m"
	ansiBold    = "\x1b[1m"
	ansiError   = "\x1b[1;31m"
	ansiWarning = "\x1b[1;33m"
	ansiNote    = "\x1b[1;36m"
	ansiHelp    = "\x1b[1;32m"
)

// palette styles the diagnostic grammar's labels. The zero value renders
// plain text, so every renderer test and non-terminal run is unstyled by
// default.
type palette struct {
	enabled bool
}

// severity styles a diagnostic label with its severity color. The
// severity vocabulary is closed (error, warning, note, help); anything
// else gets the structural bold so an unknown severity is still visible,
// never invisible.
func (p palette) severity(severity, label string) string {
	if !p.enabled {
		return label
	}
	switch severity {
	case "error":
		return ansiError + label + ansiReset
	case "warning":
		return ansiWarning + label + ansiReset
	case "note":
		return ansiNote + label + ansiReset
	case "help":
		return ansiHelp + label + ansiReset
	}
	return ansiBold + label + ansiReset
}

// bold styles a structural label: statement N:, plan:, docs:,
// name:line:column:, and the closing summaries.
func (p palette) bold(label string) string {
	if !p.enabled {
		return label
	}
	return ansiBold + label + ansiReset
}
