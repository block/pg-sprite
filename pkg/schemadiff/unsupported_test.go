package schemadiff

import (
	"errors"
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

// Every ErrUnsupportedChange the package returns from the table diff goes
// through unsupported, so it carries a cause a caller can classify. The one
// function allowed to wrap the sentinel itself is the row-security
// comparison, whose refusals are classified by the row-security path. The
// error type's own methods name the sentinel to match it and are skipped.
func TestEveryUnsupportedChangeCarriesACause(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var wrapping []string
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, entry.Name(), nil, 0)
		require.NoError(t, err)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || isUnsupportedChangeErrorMethod(fn) {
				continue
			}
			if wrapsSentinelOutsideUnsupported(fn.Body) {
				wrapping = append(wrapping, fn.Name.Name)
			}
		}
	}
	slices.Sort(wrapping)
	assert.Equal(t, []string{"DiffWithRowSecurity"}, wrapping)
}

// wrapsSentinelOutsideUnsupported reports whether body names
// ErrUnsupportedChange anywhere other than inside a call to unsupported or
// errors.Is: passed to another call, returned bare, or assigned.
func wrapsSentinelOutsideUnsupported(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && (isCallTo(call, "", "unsupported") || isCallTo(call, "errors", "Is")) {
			return false
		}
		if id, ok := n.(*ast.Ident); ok && id.Name == "ErrUnsupportedChange" {
			found = true
		}
		return true
	})
	return found
}

// isUnsupportedChangeErrorMethod reports a method declared on
// UnsupportedChangeError, by value or pointer receiver.
func isUnsupportedChangeErrorMethod(fn *ast.FuncDecl) bool {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return false
	}
	typ := fn.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	id, ok := typ.(*ast.Ident)
	return ok && id.Name == "UnsupportedChangeError"
}

func isCallTo(call *ast.CallExpr, pkg, name string) bool {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return pkg == "" && fun.Name == name
	case *ast.SelectorExpr:
		x, ok := fun.X.(*ast.Ident)
		return ok && x.Name == pkg && fun.Sel.Name == name
	}
	return false
}

// Each refusal Diff returns names its cause and still matches the sentinel,
// with the message it has always had.
func TestDiffRefusalsCarryTheirCause(t *testing.T) {
	cases := []struct {
		name    string
		live    func(*Model)
		desired func(*Model)
		cause   UnsupportedCause
	}{
		{"storage parameters", func(*Model) {}, func(m *Model) { m.Options = []string{"fillfactor=70"} }, CauseStorageParameters},
		{"persistence", func(*Model) {}, func(m *Model) { m.Unlogged = true }, CausePersistence},
		{"partition key", func(*Model) {}, func(m *Model) { m.PartitionKey = "RANGE (id)" }, CausePartitionKey},
		{"partition attachment", func(*Model) {}, func(m *Model) { m.IsPartition = true }, CausePartitionAttachment},
		{"added column collation", func(*Model) {}, func(m *Model) {
			m.Columns = append(m.Columns, Column{Name: "code", Type: "text", Collation: `"C"`})
		}, CauseAddedColumnCollation},
		{"added serial column", func(*Model) {}, func(m *Model) {
			m.Columns = append(m.Columns, Column{Name: "seq", Type: "integer", NotNull: true, SequenceDefault: true,
				Default: "nextval('events_seq_seq'::regclass)"})
		}, CauseSerialDefault},
		{"collation change", func(*Model) {}, func(m *Model) { m.Columns[1].Collation = `"C"` }, CauseCollationChange},
		{"identity change", func(*Model) {}, func(m *Model) { m.Columns[0].Identity = IdentityAlways }, CauseIdentity},
		{"generated change", func(*Model) {}, func(m *Model) { m.Columns[1].Generated = true; m.Columns[1].Default = "'x'::text" }, CauseGenerated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			live, desired := base(), base()
			tc.live(&live)
			tc.desired(&desired)
			_, err := Diff("public", live, desired)
			require.ErrorIs(t, err, ErrUnsupportedChange)
			refusal, ok := errors.AsType[*UnsupportedChangeError](err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, tc.cause, refusal.Cause)
			assert.True(t, strings.Contains(err.Error(), ErrUnsupportedChange.Error()), err.Error())
		})
	}
}

// A collation change names both collations and the declaration that keeps the
// live one, in either direction, so the reader can fix the file from the
// message alone.
func TestDiffCollationChangeNamesTheWayForward(t *testing.T) {
	collated := func(collation string) Model {
		return Model{Table: "codes", Columns: []Column{{Name: "code", Type: "text", Collation: collation}}}
	}

	_, err := Diff("public", collated(`pg_catalog."C"`), collated(""))
	require.ErrorIs(t, err, ErrUnsupportedChange)
	assert.Equal(t, `unsupported schema change: column "code" collation changes from pg_catalog."C" to the type's default, which rewrites the column and rebuilds its indexes; keep COLLATE pg_catalog."C" in the file`, err.Error())

	_, err = Diff("public", collated(""), collated(`pg_catalog."C"`))
	require.ErrorIs(t, err, ErrUnsupportedChange)
	assert.Equal(t, `unsupported schema change: column "code" collation changes from the type's default to pg_catalog."C", which rewrites the column and rebuilds its indexes; keep the column without a COLLATE clause in the file`, err.Error())
}

func TestUnsupportedCausesAreDistinct(t *testing.T) {
	causes := UnsupportedCauses()
	require.NotEmpty(t, causes)
	seen := map[UnsupportedCause]bool{}
	for _, c := range causes {
		assert.NotEmpty(t, c)
		assert.False(t, seen[c], "duplicate cause %q", c)
		seen[c] = true
	}
}

// A refusal built without a message still matches the sentinel and names
// its cause.
func TestUnsupportedChangeErrorZeroMessage(t *testing.T) {
	err := &UnsupportedChangeError{Cause: CausePersistence}
	require.ErrorIs(t, err, ErrUnsupportedChange)
	assert.Equal(t, "unsupported schema change: persistence", err.Error())
}
