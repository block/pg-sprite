package dbconn

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWithoutShadowedCatalog(t *testing.T) {
	testCases := []struct {
		name        string
		path        string
		want        string
		wantChanged bool
	}{
		{name: "default path untouched", path: `"$user", public`, want: `"$user", public`},
		{name: "empty path untouched", path: "", want: ""},
		{name: "catalog after user schema", path: "decoy, pg_catalog", want: "decoy", wantChanged: true},
		{name: "leading catalog shadows nothing", path: "pg_catalog, decoy, public", want: "pg_catalog, decoy, public"},
		{name: "catalog only", path: "pg_catalog", want: "pg_catalog"},
		{name: "repeated catalog keeps the leading entry", path: "pg_catalog, decoy, pg_catalog", want: "pg_catalog, decoy", wantChanged: true},
		{name: "role placeholder counts as a schema", path: `"$user", pg_catalog, public`, want: `"$user", public`, wantChanged: true},
		{name: "bare identifier is case-folded", path: "decoy, PG_CATALOG", want: "decoy", wantChanged: true},
		{name: "quoted catalog", path: `decoy, "pg_catalog"`, want: "decoy", wantChanged: true},
		{name: "quoted upper-case is a different schema", path: `decoy, "PG_CATALOG"`, want: `decoy, "PG_CATALOG"`},
		{name: "comma inside a quoted entry is not a separator", path: `"a, pg_catalog", public`, want: `"a, pg_catalog", public`},
		{name: "quoted comma survives a rewrite intact", path: `"a,pg_catalog", decoy, pg_catalog`, want: `"a,pg_catalog", decoy`, wantChanged: true},
		{name: "quoted entry with an escaped quote is not the catalog", path: `"we""ird", pg_catalog`, want: `"we""ird"`, wantChanged: true},
		{name: "whitespace variants", path: "  decoy ,pg_catalog,  public ", want: "decoy, public", wantChanged: true},
		{name: "empty entries are dropped from a rewritten path", path: "decoy, , pg_catalog", want: "decoy", wantChanged: true},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := withoutShadowedCatalog(tc.path)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.wantChanged, changed)
		})
	}
}

func TestLocalSearchPath(t *testing.T) {
	testCases := []struct {
		name    string
		schemas []string
		want    string
	}{
		{name: "schema then public", schemas: []string{"app", "public"}, want: `SET LOCAL search_path = "app", "public"`},
		{name: "identifiers are quoted", schemas: []string{`we"ird`, "public"}, want: `SET LOCAL search_path = "we""ird", "public"`},
		{name: "catalog after a schema is dropped", schemas: []string{"app", "pg_catalog", "public"}, want: `SET LOCAL search_path = "app", "public"`},
		{name: "leading catalog is kept", schemas: []string{"pg_catalog", "app"}, want: `SET LOCAL search_path = "pg_catalog", "app"`},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, LocalSearchPath(tc.schemas...))
		})
	}
}

// searchPathWriter matches SQL that sets search_path: SET [LOCAL|SESSION],
// set_config, or a connection-time runtime parameter.
var searchPathWriter = regexp.MustCompile(`(?i)\bSET\s+(LOCAL\s+|SESSION\s+)?search_path\b|set_config\(\s*'search_path'|RuntimeParams\["search_path"\]`)

// Every search_path pg-sprite sets in production code is built in this
// package, where the catalog-shadowing rewrite lives; a site that assembled
// its own SET LOCAL would bypass it. The test walks the production sources
// under pkg/ so a new site cannot be added without either using
// LocalSearchPath or changing this test.
func TestLocalSearchPathIsTheOnlySearchPathWriter(t *testing.T) {
	root := filepath.Join("..", "..", "pkg")
	self, err := filepath.Abs(".")
	require.NoError(t, err)
	var offenders []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		dir, err := filepath.Abs(filepath.Dir(path))
		if err != nil {
			return err
		}
		if dir == self {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(src), "\n") {
			if searchPathWriter.MatchString(line) {
				offenders = append(offenders, fmt.Sprintf("%s:%d: %s", path, i+1, strings.TrimSpace(line)))
			}
		}
		return nil
	})
	require.NoError(t, err)
	assert.Empty(t, offenders, "search_path is set outside pkg/dbconn; use dbconn.LocalSearchPath")
}
