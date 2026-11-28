package migrate

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stoppedRunChecks are the functions that read a table's checkpoint row and
// refuse over a copy-and-swap run stopped on it.
var stoppedRunChecks = []string{
	"findStoppedRun",
	"refuseOverStoppedRun",
	"refuseEmptyPlanOverStoppedRun",
	"AdmitStoppedRun",
	"AdmitPlanStoppedRun",
	"AdmitRowSecurityStoppedRun",
}

// executingPackages are the packages whose functions this package calls to
// run work on a table, each with the functions it calls from them that run
// nothing: admission checks that only parse or read the catalog, error
// codes, retry policy, and the names and preflight reads of copy-and-swap.
// Every other call into them counts as executing, so a new one is gated
// until it is listed here. A schemachange.Runner, whose Run and Discard
// change the table, is reached only through NewRunner, which counts as
// executing.
var executingPackages = map[string][]string{
	"executor": {
		"AdmitDetachPartition",
		"AdmitPartitionedForeignKeyShape",
		"AdmitPartitionedIndexShape",
		"AdmitPartitionedKey",
		"AdmitPartitionedKeyShape",
		"AdmitPartitionedUniqueIndex",
		"AdmitSequenceShape",
		"Code",
		"DefaultRetryPolicy",
		"OutcomeCode",
	},
	"schemachange": {
		"CheckCopySwap",
		"CheckPool",
		"IsDataFault",
		"MinPoolConns",
		"OldName",
		"RefusalCauseOf",
		"RefusalCauses",
		"ShadowName",
	},
}

// stoppedRunExempt are the functions that may execute, or report a plan
// converged, with no stopped-run check on the way to them, each with why
// no copy-and-swap run can be stopped under what they change.
var stoppedRunExempt = map[string]string{
	"addEnumValue": "ALTER TYPE ... ADD VALUE names a type and locks no table, and an enum label is not part of a " +
		"table's shape, so a stopped run still resumes and finishes after it",
	"RunDesiredEnumType": "an enum type's plan converges a type, not a table: its empty plan reports the type " +
		"converged, and its statements are ADD VALUE, which addEnumValue runs",
	"dropPlannedPartitionedIndex": "it drops an index of a partitioned table, and copy-and-swap refuses a partitioned " +
		"table before it builds anything, so no run can be stopped on it",
	"runCreate": "it runs only when the plan finds the table missing and preflight.CheckTableAbsent proves it absent " +
		"in the executing session, and a copy-and-swap run holds an existing table; AdmitPlanStoppedRun admits a " +
		"creating plan the same way",
}

// Every way an exported front door of this package can reach a call that
// executes work on a table — a call into pkg/executor or the
// copy-and-swap Runner — or a desired-state result that reports a plan
// converged with nothing run, passes a stopped-run check first, or ends in
// a function stoppedRunExempt names. A door wired to the executor without
// the check fails here, rather than running a statement over a stopped
// copy-and-swap run's table (ST-2).
//
// The walk follows calls within the package from each exported function.
// A check gates the statements after it in its own block and everything
// nested in them, and nothing else: a check inside an if body does not
// gate what follows the if, and one later in a function does not gate a
// call before it. That is a structural approximation of control flow, not
// a proof of it: it does not see a branch that returns before the
// check's result is used, and it resolves a method call by name alone.
func TestEveryDoorChecksForAStoppedRunBeforeItExecutes(t *testing.T) {
	w := newDoorWalk(t)
	for _, fn := range w.entries {
		w.visit(fn, nil)
	}
	assert.Empty(t, w.violations,
		"each path reaches work on a table with no stopped-run check before it: call one of %v on the way, "+
			"or name the function in stoppedRunExempt with why no stopped run can be under what it changes",
		stoppedRunChecks)
	for name := range stoppedRunExempt {
		assert.True(t, w.exemptUsed[name],
			"stoppedRunExempt names %s, which no exported door reaches executing without a check: drop the exemption", name)
	}
}

// doorWalk is the call walk over this package's non-test source.
type doorWalk struct {
	fset       *token.FileSet
	funcs      map[string]*ast.FuncDecl
	methods    map[string][]*ast.FuncDecl
	entries    []*ast.FuncDecl
	visited    map[*ast.FuncDecl]bool
	violations []string
	exemptUsed map[string]bool
}

func newDoorWalk(t *testing.T) *doorWalk {
	t.Helper()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	w := &doorWalk{
		fset:       token.NewFileSet(),
		funcs:      map[string]*ast.FuncDecl{},
		methods:    map[string][]*ast.FuncDecl{},
		visited:    map[*ast.FuncDecl]bool{},
		exemptUsed: map[string]bool{},
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(w.fset, name, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if fn.Recv == nil {
				w.funcs[fn.Name.Name] = fn
			} else {
				w.methods[fn.Name.Name] = append(w.methods[fn.Name.Name], fn)
			}
			if fn.Name.IsExported() && !slices.Contains(stoppedRunChecks, fn.Name.Name) {
				w.entries = append(w.entries, fn)
			}
		}
	}
	require.NotEmpty(t, w.entries, "no exported function found in the package source")
	for _, check := range stoppedRunChecks {
		require.Contains(t, w.funcs, check, "stoppedRunChecks names %s, which the package no longer declares", check)
	}
	return w
}

// visit walks fn's body as reached by path with no check yet passed. A
// function already walked that way adds nothing new.
func (w *doorWalk) visit(fn *ast.FuncDecl, path []string) {
	if w.visited[fn] {
		return
	}
	w.visited[fn] = true
	w.block(fn.Body.List, append(slices.Clone(path), fn.Name.Name))
}

// block walks stmts in order until one of them passes a check, which gates
// the rest.
func (w *doorWalk) block(stmts []ast.Stmt, path []string) {
	for _, s := range stmts {
		if w.stmt(s, path) {
			return
		}
	}
}

// stmt walks s and reports whether s calls a check outside its nested
// blocks, where the call runs whenever s does.
func (w *doorWalk) stmt(s ast.Stmt, path []string) bool {
	switch s := s.(type) {
	case nil:
		return false
	case *ast.BlockStmt:
		w.block(s.List, path)
		return false
	case *ast.LabeledStmt:
		return w.stmt(s.Stmt, path)
	case *ast.IfStmt:
		if w.stmt(s.Init, path) || w.node(s.Cond, path) {
			return true
		}
		w.block(s.Body.List, path)
		w.stmt(s.Else, path)
		return false
	case *ast.ForStmt:
		if w.stmt(s.Init, path) || w.node(s.Cond, path) {
			return true
		}
		w.stmt(s.Post, path)
		w.block(s.Body.List, path)
		return false
	case *ast.RangeStmt:
		if w.node(s.X, path) {
			return true
		}
		w.block(s.Body.List, path)
		return false
	case *ast.SwitchStmt:
		if w.stmt(s.Init, path) || w.node(s.Tag, path) {
			return true
		}
		w.clauses(s.Body, path)
		return false
	case *ast.TypeSwitchStmt:
		if w.stmt(s.Init, path) || w.stmt(s.Assign, path) {
			return true
		}
		w.clauses(s.Body, path)
		return false
	case *ast.SelectStmt:
		w.clauses(s.Body, path)
		return false
	default:
		return w.node(s, path)
	}
}

// clauses walks the cases of a switch or select, none of which gates
// another.
func (w *doorWalk) clauses(body *ast.BlockStmt, path []string) {
	for _, c := range body.List {
		switch c := c.(type) {
		case *ast.CaseClause:
			for _, e := range c.List {
				w.node(e, path)
			}
			w.block(c.Body, path)
		case *ast.CommClause:
			w.stmt(c.Comm, path)
			w.block(c.Body, path)
		}
	}
}

// node walks an expression or a simple statement, records each executing
// call in it, follows each package function it names, and reports whether
// it calls a check. A function literal's body is walked as a block of its
// own, and a check inside it gates nothing outside it.
func (w *doorWalk) node(n ast.Node, path []string) bool {
	if n == nil {
		return false
	}
	checked := false
	ast.Inspect(n, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			w.block(n.Body.List, path)
			return false
		case *ast.KeyValueExpr:
			// A composite literal's key is a field name, not a reference.
			checked = w.node(n.Value, path) || checked
			return false
		case *ast.CompositeLit:
			if reportsConverged(n) {
				w.sink(n.Pos(), "a DesiredResult reported executed", path)
			}
			return true
		case *ast.CallExpr:
			if called, ok := executingCall(n); ok {
				w.sink(n.Pos(), called, path)
			}
			return true
		case *ast.SelectorExpr:
			if pkg, ok := n.X.(*ast.Ident); ok {
				if _, executing := executingPackages[pkg.Name]; executing {
					// A sentinel, a type, or a function value: only a
					// call runs anything.
					return false
				}
			}
			for _, m := range w.methods[n.Sel.Name] {
				w.visit(m, path)
			}
			checked = w.node(n.X, path) || checked
			return false
		case *ast.Ident:
			if slices.Contains(stoppedRunChecks, n.Name) {
				checked = true
			} else if fn, ok := w.funcs[n.Name]; ok {
				w.visit(fn, path)
			}
			return false
		}
		return true
	})
	return checked
}

// executingCall reports whether call calls an executing package's function
// that executingPackages does not list as running nothing, and names it.
func executingCall(call *ast.CallExpr) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return "", false
	}
	reads, executing := executingPackages[pkg.Name]
	if !executing || slices.Contains(reads, sel.Sel.Name) {
		return "", false
	}
	return pkg.Name + "." + sel.Sel.Name, true
}

// reportsConverged reports whether lit is a DesiredResult whose outcome is
// one of the executed outcomes: a desired-state door built it without a
// statement verdict of its own, so it reports a plan converged.
func reportsConverged(lit *ast.CompositeLit) bool {
	if typ, ok := lit.Type.(*ast.Ident); !ok || typ.Name != "DesiredResult" {
		return false
	}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "Outcome" {
			continue
		}
		value, ok := kv.Value.(*ast.SelectorExpr)
		if ok && strings.HasPrefix(value.Sel.Name, "OutcomeExecuted") {
			return true
		}
	}
	return false
}

// sink records work reached with no check on path, unless the function it
// is in is exempt.
func (w *doorWalk) sink(pos token.Pos, what string, path []string) {
	fn := path[len(path)-1]
	if _, ok := stoppedRunExempt[fn]; ok {
		w.exemptUsed[fn] = true
		return
	}
	w.violations = append(w.violations, fmt.Sprintf("%s at %s, reached by %s",
		what, w.fset.Position(pos), strings.Join(path, " -> ")))
}
