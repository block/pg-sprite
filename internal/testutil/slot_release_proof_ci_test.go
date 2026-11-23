package testutil

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// readRepoFile reads a file at a path relative to the repository root.
func readRepoFile(t *testing.T, path ...string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(append([]string{"..", ".."}, path...)...))
	require.NoError(t, err)
	return string(body)
}

// makeRecipe returns the first line of a Makefile target's recipe.
func makeRecipe(t *testing.T, target string) string {
	t.Helper()
	match := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(target) + `:.*\n\t(.*)$`).FindStringSubmatch(readRepoFile(t, "Makefile"))
	require.Len(t, match, 2, "the Makefile has a %s recipe", target)
	return match[1]
}

// The weekly Slot Release Proof workflow runs make slot-release-proof, which
// selects one test by name. go test exits 0 with "no tests to run" when the
// name matches nothing, and the test skips itself unless SLOT_RELEASE_PROOF
// is 1, so a renamed test or a dropped variable would turn every weekly run
// green while proving nothing.
func TestSlotReleaseProofRecipeRunsAnExistingTestUnskipped(t *testing.T) {
	recipe := makeRecipe(t, "slot-release-proof")
	assert.Contains(t, recipe, "SLOT_RELEASE_PROOF=1 ", "the recipe opts the test in")

	run := regexp.MustCompile(`-run '\^(\w+)\$\$' .*\./pkg/(\w+)$`).FindStringSubmatch(recipe)
	require.Len(t, run, 3, "the recipe anchors one test name in one package")
	name, pkg := run[1], run[2]

	files, err := filepath.Glob(filepath.Join("..", "..", "pkg", pkg, "*_test.go"))
	require.NoError(t, err)
	declared := false
	for _, file := range files {
		if strings.Contains(readRepoFile(t, "pkg", pkg, filepath.Base(file)), "func "+name+"(t *testing.T)") {
			declared = true
		}
	}
	assert.True(t, declared, "the recipe runs %s, which pkg/%s must declare", name, pkg)
}

// The proof's matrix is the supported majors, the list make
// test-supported-postgres runs the suite on, so adding or dropping a major
// cannot leave the weekly proof covering a different set.
func TestSlotReleaseProofRunsOnEverySupportedMajor(t *testing.T) {
	loop := regexp.MustCompile(`for v in ([0-9 ]+); do`).FindStringSubmatch(makeRecipe(t, "test-supported-postgres"))
	require.Len(t, loop, 2, "test-supported-postgres loops over the supported majors")
	supported := strings.Fields(loop[1])

	var workflow struct {
		Jobs map[string]struct {
			Strategy struct {
				Matrix struct {
					PG []string `yaml:"pg"`
				} `yaml:"matrix"`
			} `yaml:"strategy"`
		} `yaml:"jobs"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(readRepoFile(t, ".github", "workflows", "slot-release-proof.yml")), &workflow))
	job, ok := workflow.Jobs["proof"]
	require.True(t, ok, "slot-release-proof.yml has a proof job")
	assert.Equal(t, supported, job.Strategy.Matrix.PG)
}
