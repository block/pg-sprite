package testutil

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/block/pg-sprite/pkg/capabilities"
)

// The README's PostgreSQL badge is a static image, so nothing updates it when
// a major is added or dropped. Its image and its alt text must name exactly
// the majors make test-supported-postgres runs the suite on, and those must
// be the majors CI's test job runs, so a reader deciding whether pg-sprite is
// tested on their server is not shown a stale list.
func TestReadmePostgresBadgeNamesTheSupportedMajors(t *testing.T) {
	loop := regexp.MustCompile(`for v in ([0-9 ]+); do`).FindStringSubmatch(makeRecipe(t, "test-supported-postgres"))
	require.Len(t, loop, 2, "test-supported-postgres loops over the supported majors")
	supported := strings.Fields(loop[1])

	readme := readRepoFile(t, "README.md")
	badge := regexp.MustCompile(`\[!\[PostgreSQL tested: ([0-9 |]+)\]\(https://img\.shields\.io/badge/PostgreSQL%20tested-([0-9%C7]+)-`).FindStringSubmatch(readme)
	require.Len(t, badge, 3, "the README has a PostgreSQL tested badge")
	assert.Equal(t, supported, strings.Split(badge[2], "%20%7C%20"), "the badge image")
	assert.Equal(t, supported, strings.Split(badge[1], " | "), "the badge alt text")

	assert.Equal(t, supported, ciTestMajors(t), "CI's test job runs every supported major and no other")
}

// ciTestMajors is the set of PostgreSQL majors ci.yml's test job runs: its
// pg matrix list and every include entry's pg, in ascending order.
func ciTestMajors(t *testing.T) []string {
	t.Helper()
	var workflow struct {
		Jobs map[string]struct {
			Strategy struct {
				Matrix struct {
					PG      []string `yaml:"pg"`
					Include []struct {
						PG string `yaml:"pg"`
					} `yaml:"include"`
				} `yaml:"matrix"`
			} `yaml:"strategy"`
		} `yaml:"jobs"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(readRepoFile(t, ".github", "workflows", "ci.yml")), &workflow))
	job, ok := workflow.Jobs["test"]
	require.True(t, ok, "ci.yml has a test job")
	majors := slices.Clone(job.Strategy.Matrix.PG)
	for _, entry := range job.Strategy.Matrix.Include {
		majors = append(majors, entry.PG)
	}
	slices.Sort(majors)
	return slices.Compact(majors)
}

// Every GitHub Actions badge in the README points at a workflow file that
// exists; a renamed workflow would leave a "no status" badge with every
// check green.
func TestReadmeWorkflowBadgesNameExistingWorkflows(t *testing.T) {
	badges := regexp.MustCompile(`actions/workflows/([a-z0-9-]+\.yml)/badge\.svg`).FindAllStringSubmatch(readRepoFile(t, "README.md"), -1)
	require.NotEmpty(t, badges, "the README has workflow badges")
	for _, b := range badges {
		_, err := os.Stat(filepath.Join("..", "..", ".github", "workflows", b[1]))
		assert.NoError(t, err, "the README badge names workflow %s", b[1])
	}
}

// The README's maturity badge is the project-level status, and it is preview
// exactly while some supported capability row is in preview: a change that
// takes the last preview route to ga changes the badge in the same PR.
func TestReadmeMaturityBadgeMatchesTheCapabilityMatrix(t *testing.T) {
	rows, err := capabilities.Rows()
	require.NoError(t, err)
	want := capabilities.MaturityGA
	for _, row := range rows {
		if row.Maturity == capabilities.MaturityPreview {
			want = capabilities.MaturityPreview
			break
		}
	}

	badge := regexp.MustCompile(`\[!\[Maturity: ([a-z]+)\]\(https://img\.shields\.io/badge/maturity-([a-z]+)-`).FindStringSubmatch(readRepoFile(t, "README.md"))
	require.Len(t, badge, 3, "the README has a maturity badge")
	assert.Equal(t, string(want), badge[2], "the badge image")
	assert.Equal(t, string(want), badge[1], "the badge alt text")
}
