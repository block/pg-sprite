package verdict

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cliOutputExamplesDoc is the human-facing page that documents every
// refusal-reason token; this test keeps it honest the same way
// pkg/plan/docs_test.go keeps docs/plan-report.md honest.
const cliOutputExamplesDoc = "../../docs/cli-output-examples.md"

// refusalClassesDoc is the routing contract that classifies every refusal
// reason; a reason without a class row there cannot be routed by consumers.
const refusalClassesDoc = "../../docs/refusal-classes.md"

// exitCodeLadderDocs are the pages that state the process exit-code ladder
// as a table whose first cell is the code; the README is where a CI author
// first meets it, the CLI examples page is the machine contract, and the
// passthrough design holds the reasoning behind the non-obvious cells.
var exitCodeLadderDocs = []string{
	"../../README.md",
	cliOutputExamplesDoc,
	"../../docs/lock-budgeted-passthrough.md",
}

// exitCodeLadderHeader is the header row of a ladder table; only rows under
// this header count as the ladder, so a numbered list elsewhere on the page
// (acceptance criteria, invariants) cannot satisfy the check by accident.
const exitCodeLadderHeader = "| Exit | Meaning |"

// exitCodeLadder is every process exit code the binary produces: the
// ExitCode* constants declared anywhere in this package's non-test source,
// read from the files so a new constant is in the ladder before anyone
// remembers to list it, whichever file its author opened, plus the two shell
// conventions the entry point inherits without a named constant — 0 for
// success and 1 for any error kong reports.
func exitCodeLadder(t *testing.T) map[int]struct{} {
	t.Helper()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	fset := token.NewFileSet()
	ladder := map[int]struct{}{0: {}, 1: {}}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				collectExitCodes(t, ladder, spec)
			}
		}
	}
	require.Greater(t, len(ladder), 2, "the walker found at least one declared ExitCode constant")
	return ladder
}

// collectExitCodes adds every ExitCode* name in one const spec to the
// ladder. Each name must carry its own integer literal: a spec that repeats
// the previous one implicitly, or derives from iota or another constant, is
// rejected by name rather than indexed past the end of its values.
func collectExitCodes(t *testing.T, ladder map[int]struct{}, spec ast.Spec) {
	t.Helper()
	vs, ok := spec.(*ast.ValueSpec)
	if !ok {
		return
	}
	for i, name := range vs.Names {
		if !strings.HasPrefix(name.Name, "ExitCode") {
			continue
		}
		require.Lessf(t, i, len(vs.Values), "%s states its value explicitly", name.Name)
		lit, ok := vs.Values[i].(*ast.BasicLit)
		require.Truef(t, ok && lit.Kind == token.INT, "%s is an integer literal", name.Name)
		code, err := strconv.Atoi(lit.Value)
		require.NoError(t, err)
		ladder[code] = struct{}{}
	}
}

// documentedExitCodes returns the first-cell integer of every row under
// every ladder header in the page. A doc with no ladder table yields an
// empty set, which the equality check then reports as missing every code.
func documentedExitCodes(t *testing.T, doc string) map[int]struct{} {
	t.Helper()
	codes := map[int]struct{}{}
	lines := strings.Split(doc, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, exitCodeLadderHeader) {
			continue
		}
		// Skip the header and the separator row, then read rows until the
		// table ends.
		for _, row := range lines[i+2:] {
			if !strings.HasPrefix(row, "|") {
				break
			}
			cells := strings.SplitN(row, "|", 3)
			require.Lenf(t, cells, 3, "ladder row has a first cell: %q", row)
			code, err := strconv.Atoi(strings.TrimSpace(cells[1]))
			require.NoErrorf(t, err, "ladder row's first cell is an exit code: %q", row)
			codes[code] = struct{}{}
		}
	}
	return codes
}

// Every page that states the ladder must list exactly the exit codes the
// binary produces: an exit-code constant added to this package without a row
// saying what it means for a shell gate fails here, a ladder table that
// drops a code fails, and so does a row for a code the binary no longer
// exits with.
func TestExitCodeLadderDocsListEveryCode(t *testing.T) {
	ladder := exitCodeLadder(t)
	for _, path := range exitCodeLadderDocs {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equalf(t, ladder, documentedExitCodes(t, string(raw)),
			"%s must state the exit-code ladder as a table under %q with one row per code", path, exitCodeLadderHeader)
	}
}

// Headers of the vocabulary tables the docs state as closed sets; only rows
// under a header count, so a token quoted in another table on the page is
// neither credited nor rejected by the set checks below.
const (
	refusalReasonsHeader = "| Reason | Meaning |"
	refusalSitesHeader   = "| Existing `reason` | Refusal site or shape |"
	refusalClassesHeader = "| Class | Meaning |"
	refusalOwnersHeader  = "| Owner | Work it names |"
)

// causeTableHeading is the prefix of a heading that classifies one reason by
// its own cause-keyed table rather than by a row in the site-keyed table.
const causeTableHeading = "### `"

// documentedTokens returns the backticked first-cell token of every row
// under every occurrence of the header. A page with no such table yields an
// empty set, which an equality check then reports as missing every value.
func documentedTokens(t *testing.T, doc, header string) map[string]struct{} {
	t.Helper()
	tokens := map[string]struct{}{}
	lines := strings.Split(doc, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, header) {
			continue
		}
		// Skip the header and the separator row, then read rows until the
		// table ends.
		for _, row := range lines[i+2:] {
			if !strings.HasPrefix(row, "|") {
				break
			}
			cells := strings.SplitN(row, "|", 3)
			require.Lenf(t, cells, 3, "row under %q has a first cell: %q", header, row)
			tokens[backtickedToken(t, strings.TrimSpace(cells[1]))] = struct{}{}
		}
	}
	return tokens
}

// backtickedToken strips the code fence from a `token` cell, rejecting a
// cell that is prose or a bare word so a mistyped row fails loudly rather
// than counting as an unknown value.
func backtickedToken(t *testing.T, cell string) string {
	t.Helper()
	token, ok := strings.CutPrefix(cell, "`")
	require.Truef(t, ok, "cell is a backticked token: %q", cell)
	token, ok = strings.CutSuffix(token, "`")
	require.Truef(t, ok, "cell is a backticked token: %q", cell)
	return token
}

// causeTableReasons returns the reason each cause-keyed table classifies: the
// backticked token that opens its heading.
func causeTableReasons(t *testing.T, doc string) map[string]struct{} {
	t.Helper()
	reasons := map[string]struct{}{}
	for line := range strings.SplitSeq(doc, "\n") {
		rest, ok := strings.CutPrefix(line, causeTableHeading)
		if !ok {
			continue
		}
		token, _, ok := strings.Cut(rest, "`")
		require.Truef(t, ok, "cause-table heading names a backticked reason: %q", line)
		reasons[token] = struct{}{}
	}
	return reasons
}

// tokenSet is the closed set a docs table must match, as the strings the
// page prints.
func tokenSet[T ~string](values []T) map[string]struct{} {
	set := map[string]struct{}{}
	for _, v := range values {
		set[string(v)] = struct{}{}
	}
	return set
}

// The refusal-reason table must list exactly the reasons automation can
// meet: a Reason constant added without a row saying what it means fails
// here, and so does a row for a token that is not a refusal reason — an
// executor failure code, say, which a reader would otherwise take as a
// nothing-ran refusal when the DDL has in fact committed.
func TestDocListsEveryRefusalReason(t *testing.T) {
	raw, err := os.ReadFile(cliOutputExamplesDoc)
	require.NoError(t, err)
	assert.Equal(t, tokenSet(Reasons()), documentedTokens(t, string(raw), refusalReasonsHeader),
		"docs/cli-output-examples.md must state the refusal reasons as a table under %q with one row per reason", refusalReasonsHeader)
}

// The refusal-class map must classify exactly the refusal reasons: a Reason
// constant added without a routing decision fails here, and so does a row
// or cause table for a token that is not a reason. A reason is classified
// either by rows in the site-keyed table or by its own cause-keyed table,
// whose heading names it; a reason may appear in both.
func TestRefusalClassesDocListsEveryRefusalReason(t *testing.T) {
	raw, err := os.ReadFile(refusalClassesDoc)
	require.NoError(t, err)
	doc := string(raw)
	classified := documentedTokens(t, doc, refusalSitesHeader)
	for r := range causeTableReasons(t, doc) {
		classified[r] = struct{}{}
	}
	assert.Equal(t, tokenSet(Reasons()), classified,
		"docs/refusal-classes.md must classify each refusal reason by a row under %q or a heading opening with %q, and nothing else", refusalSitesHeader, causeTableHeading)
}

// The class and owner vocabulary tables must list exactly the values the
// contract closes over: a Class or Owner constant added without a stated
// meaning and consumer action fails here, and so does a row for a value the
// contract does not carry.
func TestRefusalClassesDocListsEveryClassAndOwner(t *testing.T) {
	raw, err := os.ReadFile(refusalClassesDoc)
	require.NoError(t, err)
	doc := string(raw)
	assert.Equal(t, tokenSet(Classes()), documentedTokens(t, doc, refusalClassesHeader),
		"docs/refusal-classes.md must state the classes as a table under %q with one row per class", refusalClassesHeader)
	assert.Equal(t, tokenSet(Owners()), documentedTokens(t, doc, refusalOwnersHeader),
		"docs/refusal-classes.md must state the owners as a table under %q with one row per owner", refusalOwnersHeader)
}
