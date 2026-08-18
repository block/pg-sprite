package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/lint"
)

// devNull returns an *os.File that isTerminal reports as a terminal:
// /dev/null is a character device, exactly the mode bit the detection
// checks, so it stands in for a tty in tests.
func devNull(t *testing.T) *os.File {
	t.Helper()
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, f.Close()) })
	return f
}

func TestColorEnabledExplicitModesWin(t *testing.T) {
	// always colors even a non-terminal buffer; never stays plain even on
	// a character device with a color-friendly environment.
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	var buf strings.Builder
	assert.True(t, colorEnabled("always", &buf))
	assert.False(t, colorEnabled("never", devNull(t)))
}

func TestColorAutoDetectsTerminal(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	var buf strings.Builder
	assert.False(t, colorEnabled("auto", &buf), "a buffer is not a terminal")
	assert.True(t, colorEnabled("auto", devNull(t)), "a character device is")
}

func TestColorAutoRespectsNoColor(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "1")
	assert.False(t, colorEnabled("auto", devNull(t)))
}

func TestColorAutoRespectsDumbTerm(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "dumb")
	assert.False(t, colorEnabled("auto", devNull(t)))
}

// The colored rendering is the plain rendering with SGR sequences around
// the labels and nothing else: stripping the escape codes must reproduce
// the plain layout byte for byte, so color can never change wrapping,
// indentation, or blank-line placement.
func TestLintTextColorWrapsLabelsOnly(t *testing.T) {
	report, err := lint.Check("ALTER TABLE t DROP COLUMN legacy")
	require.NoError(t, err)

	var plain, colored strings.Builder
	require.NoError(t, writeLintText(&plain, palette{}, "change.sql", report))
	require.NoError(t, writeLintText(&colored, palette{enabled: true}, "change.sql", report))

	assert.Contains(t, colored.String(), ansiWarning)
	assert.Contains(t, colored.String(), ansiBold)
	stripped := colored.String()
	for _, seq := range []string{ansiError, ansiWarning, ansiNote, ansiHelp, ansiBold, ansiReset} {
		stripped = strings.ReplaceAll(stripped, seq, "")
	}
	assert.Equal(t, plain.String(), stripped)
}

// The JSON report is a machine contract: --color=always must not leak
// escape sequences into it.
func TestLintJSONStaysPlainUnderColorAlways(t *testing.T) {
	var out strings.Builder
	cmd := LintCmd{OutputFlags: OutputFlags{Color: "always"}, JSON: true}
	require.NoError(t, cmd.runLint(strings.NewReader("ALTER TABLE t DROP COLUMN legacy"), &out))
	assert.NotContains(t, out.String(), "\x1b[")
}

func TestPaletteSeverityStyles(t *testing.T) {
	p := palette{enabled: true}
	assert.Equal(t, ansiError+"error:"+ansiReset, p.severity("error", "error:"))
	assert.Equal(t, ansiWarning+"warning[x]:"+ansiReset, p.severity("warning", "warning[x]:"))
	assert.Equal(t, ansiNote+"note:"+ansiReset, p.severity("note", "note:"))
	assert.Equal(t, ansiHelp+"help:"+ansiReset, p.severity("help", "help:"))
	// An unknown severity is still visible, never invisible.
	assert.Equal(t, ansiBold+"odd:"+ansiReset, p.severity("odd", "odd:"))
	assert.Equal(t, "error:", palette{}.severity("error", "error:"))
}
