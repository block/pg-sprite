package testutil

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// The integration suite's race run is split across two CI jobs, the
// Makefile's test-db-slow and test-db-rest, which together run what test-db
// runs. all-green passes when no job failed, so a half dropped from the
// matrix, or one that lost its -race flag, would leave packages off the race
// gate with every check green. The own-server tests run in exactly one half:
// none would leave them on no gate in the race run, and both would run them
// twice.
func TestCIRaceJobRunsBothHalvesOfTheSuite(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yml"))
	require.NoError(t, err)
	var workflow struct {
		Jobs map[string]struct {
			Strategy struct {
				Matrix struct {
					Include []struct {
						Race      string `yaml:"race"`
						Target    string `yaml:"target"`
						OwnServer bool   `yaml:"own_server"`
					} `yaml:"include"`
				} `yaml:"matrix"`
			} `yaml:"strategy"`
		} `yaml:"jobs"`
	}
	require.NoError(t, yaml.Unmarshal(body, &workflow))
	job, ok := workflow.Jobs["test"]
	require.True(t, ok, "ci.yml has a test job")

	racedTargets := map[string]bool{}
	ownServerHalves := 0
	for _, entry := range job.Strategy.Matrix.Include {
		if entry.Race != "-race" {
			continue
		}
		racedTargets[entry.Target] = true
		if entry.OwnServer {
			ownServerHalves++
		}
	}
	assert.Equal(t, map[string]bool{"test-db-slow": true, "test-db-rest": true}, racedTargets,
		"the race job must run both halves of the suite, each under -race")
	assert.Equal(t, 1, ownServerHalves,
		"exactly one half of the race job must run make test-own-server")
}
