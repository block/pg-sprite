package verdict

import (
	"fmt"
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
// ExitCode* constants declared in verdict.go, read from the source so a new
// constant is in the ladder before anyone remembers to list it, plus the two
// shell conventions the entry point inherits without a named constant — 0
// for success and 1 for any error kong reports.
func exitCodeLadder(t *testing.T) map[int]struct{} {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "verdict.go", nil, parser.SkipObjectResolution)
	require.NoError(t, err)

	ladder := map[int]struct{}{0: {}, 1: {}}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "ExitCode") {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				require.Truef(t, ok && lit.Kind == token.INT, "%s is an integer literal", name.Name)
				code, err := strconv.Atoi(lit.Value)
				require.NoError(t, err)
				ladder[code] = struct{}{}
			}
		}
	}
	require.Contains(t, ladder, ExitCodeRefused, "the walker recognises the declared constants")
	require.Contains(t, ladder, ExitCodeAcceptedBlocking, "the walker recognises the declared constants")
	return ladder
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
// binary produces: an exit-code constant added to verdict.go without a row
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

// Every refusal reason automation can meet must be documented: a Reason
// constant added without a row in the doc's refusal-reason table fails here.
func TestDocListsEveryRefusalReason(t *testing.T) {
	raw, err := os.ReadFile(cliOutputExamplesDoc)
	require.NoError(t, err)
	doc := string(raw)
	for _, r := range Reasons() {
		assert.Contains(t, doc, fmt.Sprintf("| `%s` |", string(r)),
			"docs/cli-output-examples.md is missing a refusal-reason row for %q", r)
	}
}

// Every refusal reason must be classified: a Reason constant added without a
// place in the refusal-class map fails here, so a new reason cannot land
// without a routing decision. A reason is classified either by a row in the
// site-keyed table or by its own cause-keyed table, whose heading names it.
func TestRefusalClassesDocListsEveryRefusalReason(t *testing.T) {
	raw, err := os.ReadFile(refusalClassesDoc)
	require.NoError(t, err)
	doc := string(raw)
	for _, r := range Reasons() {
		row := fmt.Sprintf("| `%s` |", string(r))
		causeTable := fmt.Sprintf("### `%s`, keyed on", string(r))
		assert.True(t, strings.Contains(doc, row) || strings.Contains(doc, causeTable),
			"docs/refusal-classes.md has neither a class row nor a cause table for refusal reason %q", r)
	}
}

// Every class and owner the contract closes over must be documented: a
// Class or Owner constant added without a row in the refusal-classes
// vocabulary tables fails here, so a new value cannot land without a stated
// meaning and consumer action.
func TestRefusalClassesDocListsEveryClassAndOwner(t *testing.T) {
	raw, err := os.ReadFile(refusalClassesDoc)
	require.NoError(t, err)
	doc := string(raw)
	for _, c := range Classes() {
		assert.Contains(t, doc, fmt.Sprintf("| `%s` |", string(c)),
			"docs/refusal-classes.md is missing a vocabulary row for class %q", c)
	}
	for _, o := range Owners() {
		assert.Contains(t, doc, fmt.Sprintf("| `%s` |", string(o)),
			"docs/refusal-classes.md is missing a vocabulary row for owner %q", o)
	}
}
