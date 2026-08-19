package cli

import (
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/lint"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/suggest"
	"github.com/block/pg-sprite/pkg/verdict"
)

// stripSGR removes every ANSI SGR sequence the palette can emit, so the
// parity tests can compare a colored rendering with its plain layout.
func stripSGR(s string) string {
	for _, seq := range []string{ansiError, ansiWarning, ansiNote, ansiHelp, ansiBold, ansiReset} {
		s = strings.ReplaceAll(s, seq, "")
	}
	return s
}

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
	assert.Equal(t, plain.String(), stripSGR(colored.String()))
}

// saferIdiomReport builds the substitution fixture the renderer parity
// tests share: one blocking statement the planner replaces with its
// concurrent sequence, exercising the warning, note, docs, and plan labels.
func saferIdiomReport(source plan.Source) plan.Report {
	report := plan.NewReport(source)
	report.Schema, report.Table, report.ServerVersion = "public", "users", "16.10"
	report.Disposition = router.DispositionExecute
	report.Statements = append(report.Statements, plan.Statement{
		SQL:         `ALTER TABLE "users" ADD CONSTRAINT "u" UNIQUE ("email")`,
		Route:       planner.RouteNative,
		Backend:     router.BackendNative,
		Disposition: router.DispositionExecute,
		Decisions: []planner.Decision{{
			Operation: "add unique constraint",
			Route:     planner.RouteNative,
			Reason:    planner.ReasonSaferIdiom,
		}},
		ExecSQL: []string{
			`CREATE UNIQUE INDEX CONCURRENTLY "u" ON "users" ("email")`,
			`ALTER TABLE "users" ADD CONSTRAINT "u" UNIQUE USING INDEX "u"`,
		},
		Execution: planner.ExecutionAutocommit,
	})
	return report
}

// The dry-run renderer shares lint's invariant: stripping the escape codes
// from the colored rendering must reproduce the plain layout byte for byte.
func TestDryRunTextColorWrapsLabelsOnly(t *testing.T) {
	report := saferIdiomReport(plan.SourceAlter)

	var plain, colored strings.Builder
	require.NoError(t, writeDryRunText(&plain, palette{}, report))
	require.NoError(t, writeDryRunText(&colored, palette{enabled: true}, report))

	assert.Contains(t, colored.String(), ansiWarning)
	assert.Contains(t, colored.String(), ansiBold)
	assert.Equal(t, plain.String(), stripSGR(colored.String()))
}

// The diff renderer shares lint's invariant: stripping the escape codes
// from the colored rendering must reproduce the plain layout byte for byte.
func TestDiffTextColorWrapsLabelsOnly(t *testing.T) {
	report := saferIdiomReport(plan.SourceDiff)
	exists := true
	report.TableExists = &exists

	var plain, colored strings.Builder
	require.NoError(t, writeDiffText(&plain, palette{}, report))
	require.NoError(t, writeDiffText(&colored, palette{enabled: true}, report))

	assert.Contains(t, colored.String(), ansiWarning)
	assert.Contains(t, colored.String(), ansiBold)
	assert.Equal(t, plain.String(), stripSGR(colored.String()))
}

// The suggest renderer shares lint's invariant: stripping the escape codes
// from the colored rendering must reproduce the plain layout byte for byte.
func TestSuggestTextColorWrapsLabelsOnly(t *testing.T) {
	report, err := suggest.Advise("CREATE INDEX t_c_idx ON t (c)")
	require.NoError(t, err)

	var plain, colored strings.Builder
	require.NoError(t, writeSuggestText(&plain, palette{}, "change.sql", report))
	require.NoError(t, writeSuggestText(&colored, palette{enabled: true}, "change.sql", report))

	assert.Contains(t, colored.String(), ansiBold)
	assert.Equal(t, plain.String(), stripSGR(colored.String()))
}

// fullVerdict populates every exported field of Verdict at once —
// semantically impossible as a single verdict, which is the point: it
// makes the renderer parity lock structural rather than fixture-vigilant.
// The reflection check fails when a field is added to Verdict without
// setting it here, so a field rendered by only one of the two renderers
// cannot slip past the parity test in a shape no realistic fixture sets.
func fullVerdict(t *testing.T) verdict.Verdict {
	t.Helper()
	v := verdict.Verdict{
		Outcome:       verdict.OutcomeFailed,
		Reason:        verdict.ReasonIndexStatement,
		Cause:         verdict.CauseLockBudget,
		Code:          "lock-budget-exceeded",
		FailedStep:    2,
		FailedStepSQL: `ALTER TABLE "t" VALIDATE CONSTRAINT "c"`,
		Attempts:      3,
		Statement:     "ALTER TABLE t ADD COLUMN c int",
		Table:         "public.t",
		Detail:        "every field populated for the renderer parity lock",
		SaferIdiom:    "DROP INDEX CONCURRENTLY",
		ExecutedSQL:   []string{`ALTER TABLE "t" ADD CONSTRAINT "c" CHECK (x > 0) NOT VALID`},
		Forced:        true,
	}
	rv := reflect.ValueOf(v)
	for i := range rv.NumField() {
		require.False(t, rv.Field(i).IsZero(),
			"Verdict field %s is zero in the all-fields fixture; set it so the renderer parity lock covers it",
			rv.Type().Field(i).Name)
	}
	return v
}

// The styled verdict rendering is pinned to verdict.String twice over: the
// plain rendering must match it byte for byte (plus the trailing newline
// emit always wrote), and stripping the escape codes from the colored
// rendering must reproduce the plain layout — so the two renderings cannot
// drift and color can never change the verdict's shape.
func TestVerdictTextColorWrapsLabelsOnly(t *testing.T) {
	verdicts := []verdict.Verdict{
		{
			Outcome:   verdict.OutcomeExecuted,
			Table:     "public.users",
			Statement: `ALTER TABLE "users" ADD CONSTRAINT "u" UNIQUE ("email")`,
			ExecutedSQL: []string{
				`CREATE UNIQUE INDEX CONCURRENTLY "u" ON "users" ("email")`,
				`ALTER TABLE "users" ADD CONSTRAINT "u" UNIQUE USING INDEX "u"`,
			},
		},
		{
			Outcome:    verdict.OutcomeRefused,
			Reason:     verdict.ReasonIndexStatement,
			Statement:  "DROP INDEX i",
			Detail:     "a plain DROP INDEX takes ACCESS EXCLUSIVE on the table; the concurrent drop does not",
			SaferIdiom: "DROP INDEX CONCURRENTLY",
		},
		{
			Outcome:       verdict.OutcomeFailed,
			Code:          "lock-budget-exceeded",
			Statement:     "ALTER TABLE t ADD COLUMN c int",
			Attempts:      3,
			Forced:        true,
			FailedStep:    2,
			FailedStepSQL: `ALTER TABLE "t" VALIDATE CONSTRAINT "c"`,
			ExecutedSQL:   []string{`ALTER TABLE "t" ADD CONSTRAINT "c" CHECK (x > 0) NOT VALID`},
		},
		{Outcome: verdict.Outcome("mystery"), Statement: "SELECT 1"},
		fullVerdict(t),
	}
	for _, v := range verdicts {
		var plain, colored strings.Builder
		require.NoError(t, writeVerdictText(&plain, palette{}, v))
		require.NoError(t, writeVerdictText(&colored, palette{enabled: true}, v))

		assert.Equal(t, v.String()+"\n", plain.String())
		assert.NotEqual(t, plain.String(), colored.String())
		assert.Equal(t, plain.String(), stripSGR(colored.String()))
	}
}

// The machine outputs are contracts: --color=always must not leak escape
// sequences into any of them. The offline commands run at the command
// level with the flag set; the database-backed surfaces (diff --json,
// diff --sql, migrate --dry-run --json) are exercised through the exact
// renderers their commands dispatch to, which take no palette by
// construction — this pins that property against a refactor that threads
// one "for consistency".
func TestMachineOutputsStayPlainUnderColorAlways(t *testing.T) {
	dryRunReport := saferIdiomReport(plan.SourceAlter)
	diffReport := saferIdiomReport(plan.SourceDiff)
	exists := true
	diffReport.TableExists = &exists

	cases := []struct {
		name string
		emit func(out io.Writer) error
	}{
		{"lint --json", func(out io.Writer) error {
			cmd := LintCmd{OutputFlags: OutputFlags{Color: "always"}, JSON: true}
			return cmd.runLint(strings.NewReader("ALTER TABLE t DROP COLUMN legacy"), out)
		}},
		{"suggest --json", func(out io.Writer) error {
			cmd := SuggestCmd{OutputFlags: OutputFlags{Color: "always"}, JSON: true}
			return cmd.runSuggest(strings.NewReader("CREATE INDEX t_c_idx ON t (c)"), out)
		}},
		{"diff --json", func(out io.Writer) error { return writeJSON(out, diffReport) }},
		{"diff --sql", func(out io.Writer) error { return writePlanText(out, diffReport) }},
		{"migrate --dry-run --json", func(out io.Writer) error { return writeJSON(out, dryRunReport) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			require.NoError(t, tc.emit(&out))
			require.NotEmpty(t, out.String())
			assert.NotContains(t, out.String(), "\x1b[")
		})
	}
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
