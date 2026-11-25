package schemadiff

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RenderRefusal names the verdict inside a wrapped Render error, and reports
// nothing for an error that is not one of the renderer's verdicts.
func TestRenderRefusalNamesTheVerdict(t *testing.T) {
	err := fmt.Errorf("table %q: %w", "sessions", ErrUnrenderableUnlogged)
	verdict, ok := RenderRefusal(err)
	require.True(t, ok)
	assert.Equal(t, ErrUnrenderableUnlogged, verdict)

	verdict, ok = RenderRefusal(errors.New("connection refused"))
	assert.False(t, ok)
	assert.Nil(t, verdict)
}

// Every ErrUnrenderable verdict the package declares is one RenderRefusal
// recognizes, so a new verdict cannot reach a caller unnamed.
func TestRenderRefusalsCoverEveryUnrenderableVerdict(t *testing.T) {
	sources, err := filepath.Glob("*.go")
	require.NoError(t, err)

	listed := map[string]bool{}
	for _, verdict := range renderRefusals() {
		listed[verdict.Error()] = true
	}
	declared := map[string]error{
		"ErrUnrenderableDefault":                ErrUnrenderableDefault,
		"ErrUnrenderablePartition":              ErrUnrenderablePartition,
		"ErrUnrenderableInheritance":            ErrUnrenderableInheritance,
		"ErrUnrenderableForeignKey":             ErrUnrenderableForeignKey,
		"ErrUnrenderableUnlogged":               ErrUnrenderableUnlogged,
		"ErrUnrenderableCollation":              ErrUnrenderableCollation,
		"ErrUnrenderableVirtual":                ErrUnrenderableVirtual,
		"ErrUnrenderableToastOptions":           ErrUnrenderableToastOptions,
		"ErrUnrenderableSequenceOptions":        ErrUnrenderableSequenceOptions,
		"ErrUnrenderableMergedConstraints":      ErrUnrenderableMergedConstraints,
		"ErrUnrenderableUnattachedIndex":        ErrUnrenderableUnattachedIndex,
		"ErrUnrenderablePartitionedRowSecurity": ErrUnrenderablePartitionedRowSecurity,
		"ErrUnrenderableRowSecurity":            ErrUnrenderableRowSecurity,
	}
	fileSet := token.NewFileSet()
	for _, source := range sources {
		if strings.HasSuffix(source, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, source, nil, 0)
		require.NoError(t, err)
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				for _, name := range spec.(*ast.ValueSpec).Names {
					if !strings.HasPrefix(name.Name, "ErrUnrenderable") {
						continue
					}
					verdict, known := declared[name.Name]
					require.True(t, known, "%s is a new render verdict: add it to renderRefusals and to this test", name.Name)
					assert.True(t, listed[verdict.Error()], "%s is missing from renderRefusals", name.Name)
				}
			}
		}
	}
}
