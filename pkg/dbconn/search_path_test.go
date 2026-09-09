package dbconn

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
		{name: "escaped quote inside a quoted entry", path: `"we""ird", pg_catalog`, want: `"we""ird"`, wantChanged: true},
		{name: "whitespace variants", path: "  decoy ,pg_catalog,  public ", want: "decoy, public", wantChanged: true},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := withoutShadowedCatalog(tc.path)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.wantChanged, changed)
		})
	}
}
