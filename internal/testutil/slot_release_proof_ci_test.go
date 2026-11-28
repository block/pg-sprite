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

// GitHub opens no issue for a failed scheduled run, so the workflow's report
// job does. It must wait for every matrix job and run whatever their outcome
// (always()), or a failed proof would skip the very job that reports it; it
// must run only on the schedule, so a branch's red run is not filed as main's;
// and only it may write issues. The script finds the failed majors by the
// proof jobs' names, so the name format is pinned too.
func TestSlotReleaseProofReportJobFilesScheduledFailures(t *testing.T) {
	type job struct {
		Name     string            `yaml:"name"`
		Needs    []string          `yaml:"needs"`
		If       string            `yaml:"if"`
		Perms    map[string]string `yaml:"permissions"`
		Strategy struct {
			Matrix map[string]any `yaml:"matrix"`
		} `yaml:"strategy"`
		Steps []struct {
			Env map[string]string `yaml:"env"`
			Run string            `yaml:"run"`
		} `yaml:"steps"`
	}
	var workflow struct {
		Perms map[string]string `yaml:"permissions"`
		Jobs  map[string]job    `yaml:"jobs"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(readRepoFile(t, ".github", "workflows", "slot-release-proof.yml")), &workflow))
	report, ok := workflow.Jobs["report"]
	require.True(t, ok, "slot-release-proof.yml has a report job")

	var matrixJobs []string
	for name, j := range workflow.Jobs {
		if j.Strategy.Matrix != nil {
			matrixJobs = append(matrixJobs, name)
		}
	}
	require.NotEmpty(t, matrixJobs, "the workflow has a matrix job")
	assert.ElementsMatch(t, matrixJobs, report.Needs, "the report job needs every matrix job")
	assert.Equal(t, "github.event_name == 'schedule' && always()", report.If)

	assert.Equal(t, map[string]string{"contents": "read"}, workflow.Perms, "the workflow-level token stays read-only")
	assert.Equal(t, map[string]string{"contents": "read", "actions": "read", "issues": "write"}, report.Perms)
	for name, j := range workflow.Jobs {
		if name != "report" {
			assert.NotContains(t, j.Perms, "issues", "only the report job touches issues; %s does", name)
		}
	}

	assert.Equal(t, "slot release proof (PostgreSQL ${{ matrix.pg }})", workflow.Jobs["proof"].Name,
		"scripts/slot-release-proof-issue.sh reads the failed majors from this name")
	var reported bool
	for _, step := range report.Steps {
		if step.Run == `scripts/slot-release-proof-issue.sh "$PROOF_RESULT"` {
			reported = true
			assert.Equal(t, "${{ needs.proof.result }}", step.Env["PROOF_RESULT"], "the script is given the proof's result")
		}
	}
	assert.True(t, reported, "the report job runs scripts/slot-release-proof-issue.sh")
}
