package testutil

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The README's PostgreSQL badge is a static image, so nothing updates it when
// a major is added or dropped. It must name exactly the majors make
// test-supported-postgres runs the suite on, so a reader deciding whether
// pg-sprite supports their server is not shown a stale list.
func TestReadmePostgresBadgeNamesTheSupportedMajors(t *testing.T) {
	loop := regexp.MustCompile(`for v in ([0-9 ]+); do`).FindStringSubmatch(makeRecipe(t, "test-supported-postgres"))
	require.Len(t, loop, 2, "test-supported-postgres loops over the supported majors")
	supported := strings.Fields(loop[1])

	badge := regexp.MustCompile(`img\.shields\.io/badge/PostgreSQL-([0-9%C7]+)-`).FindStringSubmatch(readRepoFile(t, "README.md"))
	require.Len(t, badge, 2, "the README has a PostgreSQL badge")
	assert.Equal(t, supported, strings.Split(badge[1], "%20%7C%20"))
}
