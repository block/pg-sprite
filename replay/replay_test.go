package replay_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The replay scripts' pre-flight guarantees and divergence reporting, run
// hermetically: pg-sprite, the harness, and docker are replaced by fakes, so
// no database or container is involved. The fakes key off markers in the
// statement text — REFUSE makes pg-sprite refuse (exit 2), ENGINEFAIL makes
// it fail (exit 1), and FAILAPPLY makes the harness's psql reject the SQL.

const fixtureCommit = "1111111111111111111111111111111111111111"

// fakePgSprite answers `migrate --json --alter <sql>` from the statement text.
const fakePgSprite = `#!/usr/bin/env bash
sql=""
while [ $# -gt 0 ]; do
    if [ "$1" = "--alter" ]; then sql="$2"; shift; fi
    shift
done
case "$sql" in
*REFUSE*) echo '{"outcome":"refused","reason":"needs-cic","class":"by-design"}'; exit 2 ;;
*ENGINEFAIL*) echo 'engine failed' >&2; echo '{"outcome":"failed"}'; exit 1 ;;
*) echo '{"outcome":"executed-natively"}'; exit 0 ;;
esac
`

// fakeHarness stands in for harness.sh: psql reads the SQL from stdin and
// rejects it when it carries FAILAPPLY; every other subcommand succeeds.
const fakeHarness = `#!/usr/bin/env bash
case "$2" in
dsn) echo "postgres://fake" ;;
psql) grep -q FAILAPPLY && exit 1; exit 0 ;;
esac
exit 0
`

const fakeDocker = "#!/usr/bin/env bash\nexit 0\n"

type fixture struct {
	dir     string // the copied replay directory
	project string // replay/<project>
}

// The default corpus: a baseline the harness applies wholesale, then one
// executed step and one refused step.
func defaultCorpus() map[string]string {
	return map[string]string{
		"0001_base.sql": "CREATE TABLE t (id int PRIMARY KEY);\n",
		"0002_add.sql": `-- add a column
ALTER TABLE t ADD COLUMN a int;
`,
		"0003_index.sql": "CREATE INDEX REFUSE_idx ON t (a);\n",
	}
}

const defaultManifest = `0002 2-2 execute
0003 1-1 refuse:needs-cic:by-design
`

// newFixture copies the real replay scripts next to fake collaborators and
// writes a fetched, pinned corpus, as fetch.sh would have left it.
func newFixture(t *testing.T, corpus map[string]string, manifest string) fixture {
	t.Helper()
	for _, tool := range []string{"bash", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found: %v", tool, err)
		}
	}
	dir := t.TempDir()
	for _, f := range []string{"replay.sh", "common.sh", "coverage.py"} {
		src, err := os.ReadFile(f)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, f), src, 0o755))
	}
	writeExec(t, filepath.Join(dir, "harness.sh"), fakeHarness)
	writeExec(t, filepath.Join(dir, "bin", "pg-sprite"), fakePgSprite)
	writeExec(t, filepath.Join(dir, "bin", "docker"), fakeDocker)

	project := filepath.Join(dir, "fix")
	require.NoError(t, os.MkdirAll(filepath.Join(project, "corpus"), 0o755))
	files := []string{"0001_base.sql", "0002_add.sql", "0003_index.sql"}
	conf := `REPO="example/fix"
COMMIT="` + fixtureCommit + `"
MIGRATIONS_PATH="migrations"
PG_IMAGE="postgres:16"
PORT=5499
BASELINE="0001_base.sql"
FILES=(` + strings.Join(files, " ") + `)
`
	require.NoError(t, os.WriteFile(filepath.Join(project, "project.conf"), []byte(conf), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(project, "assessment.tsv"), []byte(manifest), 0o644))

	var pin strings.Builder
	pin.WriteString(fixtureCommit + "\n")
	for _, f := range files {
		body := corpus[f]
		require.NoError(t, os.WriteFile(filepath.Join(project, "corpus", f), []byte(body), 0o644))
		sum := sha256.Sum256([]byte(body))
		pin.WriteString(hex.EncodeToString(sum[:]) + "  " + f + "\n")
	}
	require.NoError(t, os.WriteFile(filepath.Join(project, "corpus", ".pin"), []byte(pin.String()), 0o644))
	return fixture{dir: dir, project: project}
}

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o755))
}

// run executes replay.sh and returns its combined output and exit code.
func (f fixture) run(t *testing.T) (string, int) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "bash", filepath.Join(f.dir, "replay.sh"), "fix")
	cmd.Env = append(os.Environ(),
		"PGS="+filepath.Join(f.dir, "bin", "pg-sprite"),
		"PATH="+filepath.Join(f.dir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr, "replay.sh did not run: %s", out)
	return string(out), exitErr.ExitCode()
}

func TestReplayPassesWhenEveryVerdictMatchesItsPin(t *testing.T) {
	out, code := newFixture(t, defaultCorpus(), defaultManifest).run(t)
	assert.Equal(t, 0, code, out)
	assert.Contains(t, out, "replay complete: every assessed statement matched its expected verdict")
}

// A refused last row whose psql advance fails diverges the run. The row must
// read FAIL and the divergence must be named even though no later row is SKIP.
func TestReplayReportsAFailedAdvanceOnTheLastRow(t *testing.T) {
	corpus := defaultCorpus()
	corpus["0003_index.sql"] = "CREATE INDEX REFUSE_FAILAPPLY_idx ON t (a);\n"
	out, code := newFixture(t, corpus, defaultManifest).run(t)
	assert.Equal(t, 1, code, out)
	assert.Regexp(t, `0003 +1-1 +refuse:needs-cic:by-design .*\(exit 2\) +FAIL`, out)
	assert.Contains(t, out, "0003 1-1: psql could not apply the refused statement; state diverged here")
	assert.Contains(t, out, "1 row(s) mismatched their pin")
	assert.Contains(t, out, "state diverged at 0003 1-1")
}

// An exit-1 engine failure may have committed a prefix, so the run diverges
// and every later row is SKIP rather than evidence.
func TestReplayMarksRowsAfterAnEngineFailureUnassessable(t *testing.T) {
	corpus := defaultCorpus()
	corpus["0002_add.sql"] = `-- add a column
ALTER TABLE t ADD COLUMN ENGINEFAIL int;
`
	out, code := newFixture(t, corpus, defaultManifest).run(t)
	assert.Equal(t, 1, code, out)
	assert.Regexp(t, `0003 +1-1 +\S+ +unassessable \(diverged at 0002 2-2\) +SKIP`, out)
	assert.Contains(t, out, "state diverged at 0002 2-2")
}

// Each pre-flight refusal stops the run before any statement reaches the
// fake engine, so the results table never prints.
func TestReplayRefusesACorpusThatIsNotThePinnedOne(t *testing.T) {
	cases := []struct {
		name    string
		corrupt func(t *testing.T, f fixture)
		want    string
	}{
		{
			name: "stale pin",
			corrupt: func(t *testing.T, f fixture) {
				pin := filepath.Join(f.project, "corpus", ".pin")
				body, err := os.ReadFile(pin)
				require.NoError(t, err)
				stale := strings.Replace(string(body), fixtureCommit, strings.Repeat("2", 40), 1)
				require.NoError(t, os.WriteFile(pin, []byte(stale), 0o644))
			},
			want: "corpus was fetched at 222222222222 but project.conf pins 111111111111",
		},
		{
			name: "locally edited file",
			corrupt: func(t *testing.T, f fixture) {
				require.NoError(t, os.WriteFile(filepath.Join(f.project, "corpus", "0002_add.sql"),
					[]byte("ALTER TABLE t ADD COLUMN b int;\n"), 0o644))
			},
			want: "corpus/0002_add.sql does not match what fetch.sh fetched",
		},
		{
			name: "missing file",
			corrupt: func(t *testing.T, f fixture) {
				require.NoError(t, os.Remove(filepath.Join(f.project, "corpus", "0003_index.sql")))
			},
			want: "missing corpus/0003_index.sql",
		},
		{
			name: "file outside FILES",
			corrupt: func(t *testing.T, f fixture) {
				require.NoError(t, os.WriteFile(filepath.Join(f.project, "corpus", "0002_add_old.sql"),
					[]byte("ALTER TABLE t ADD COLUMN a int;\n"), 0o644))
			},
			want: "corpus/0002_add_old.sql is not in project.conf FILES",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, defaultCorpus(), defaultManifest)
			tc.corrupt(t, f)
			out, code := f.run(t)
			assert.Equal(t, 1, code, out)
			assert.Contains(t, out, tc.want)
			assert.NotContains(t, out, "== per-statement results")
		})
	}
}

// The manifest must claim every substantive line exactly once. A `/* */`
// comment is substantive: it must be claimed, never silently skipped.
func TestReplayRefusesAManifestThatDoesNotTileTheCorpus(t *testing.T) {
	cases := []struct {
		name     string
		corpus   func() map[string]string
		manifest string
		want     string
	}{
		{
			name:     "unclaimed statement",
			corpus:   defaultCorpus,
			manifest: "0002 2-2 execute\n",
			want:     "0003_index.sql: line 1 is substantive but no manifest row claims it",
		},
		{
			name:   "overlapping ranges",
			corpus: defaultCorpus,
			manifest: `0002 1-2 execute
0002 2-2 execute
0003 1-1 refuse:needs-cic:by-design
`,
			want: "0002_add.sql: range 2-2 overlaps or precedes an earlier range",
		},
		{
			name: "unclaimed block comment",
			corpus: func() map[string]string {
				c := defaultCorpus()
				c["0002_add.sql"] = `/* add a column */
ALTER TABLE t ADD COLUMN a int;
`
				return c
			},
			manifest: defaultManifest,
			want:     "0002_add.sql: line 1 is substantive but no manifest row claims it",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, code := newFixture(t, tc.corpus(), tc.manifest).run(t)
			assert.Equal(t, 1, code, out)
			assert.Contains(t, out, tc.want)
			assert.Contains(t, out, "manifest does not tile the corpus")
		})
	}
}
