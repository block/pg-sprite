package testutil

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every event trigger a test installs is database-global, so its function
// must outlive every transaction that may have cached it. Only this
// package's drained drop does that; an inline CREATE EVENT TRIGGER in
// another package's test would bring back the cross-package failure.
func TestEventTriggersAreInstalledOnlyThroughTestutil(t *testing.T) {
	root := filepath.Join("..", "..")
	self := filepath.Join(root, "internal", "testutil") + string(filepath.Separator)
	var inline []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") || strings.HasPrefix(path, self) {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(body), "CREATE EVENT TRIGGER") {
			inline = append(inline, filepath.ToSlash(strings.TrimPrefix(path, root+string(filepath.Separator))))
		}
		return nil
	})
	require.NoError(t, err)
	assert.Empty(t, inline, "install event triggers through testutil.InstallEventTrigger so their functions are dropped only after the drain")
}
