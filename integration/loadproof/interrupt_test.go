package loadproof_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// interruptible returns the run's context, which the operator's SIGINT or
// SIGTERM cancels. Left to its default, either signal ends the test process
// at once without its cleanups, leaving the binary running, its
// replication slot pinning WAL, and the seeded table in place; caught, it
// cancels the run instead: the binary is killed and waited for, the test
// fails, and the cleanups remove what the run left. The handler stays in
// place until the last cleanup has run, so a second interrupt does not cut
// the cleanups short.
func interruptible(t *testing.T) context.Context {
	t.Helper()
	ctx, stop := signal.NotifyContext(t.Context(), os.Interrupt, syscall.SIGTERM)
	t.Cleanup(stop)
	return ctx
}

// interruptHelperDir names the directory the interrupted run writes its
// markers to; the run is skipped when it is unset, so it only runs as the
// subprocess TestInterruptKillsTheBinaryAndRunsTheCleanups starts.
const interruptHelperDir = "LOAD_PROOF_INTERRUPT_HELPER_DIR"

// Markers of the interrupted run: started, holding its process ID, written
// by the stand-in binary itself once it runs, and cleanedUp, written by the
// run's cleanup.
const (
	startedMarker   = "started"
	cleanedUpMarker = "cleaned-up"
)

// Deadlines of the interrupt test. Neither bounds a run under proof.
const (
	// helperStartDeadline bounds the wait for the subprocess to reach the
	// binary: one test-binary start.
	helperStartDeadline = 30 * time.Second
	// helperExitDeadline bounds the wait for the interrupted subprocess to
	// exit: a kill, a wait, and a cleanup that writes one file. A binary
	// the interrupt did not kill runs far longer.
	helperExitDeadline = 30 * time.Second
	// helperPollInterval paces the wait for the started marker.
	helperPollInterval = 50 * time.Millisecond
	// interruptedBinaryArg is how long, in seconds, the stand-in binary
	// runs unless it is killed: past every deadline above.
	interruptedBinaryArg = "600"
)

// An interrupt sent to the proof alone — not to the binary it runs, as a
// SIGTERM to the test binary's own process is — kills the binary, waits for
// it, and runs the test's cleanups, which is where the run's replication
// slot and seeded table are removed. Both signals the proof catches are
// sent. The run is a subprocess of this test, so the signal reaches no
// other test, and its binary is a sleep that only a kill ends in time.
func TestInterruptKillsTheBinaryAndRunsTheCleanups(t *testing.T) {
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) { interruptRun(t, sig) })
	}
}

// interruptRun starts the interrupted run, waits for its stand-in binary to
// be running, sends sig to the run, and checks that the run failed its test,
// killed the binary, and ran its cleanups.
func interruptRun(t *testing.T, sig os.Signal) {
	dir := t.TempDir()
	shell, err := exec.LookPath("sh")
	require.NoError(t, err)
	// The test's context ending kills the run's whole process group, the
	// stand-in binary included, so the goroutine waiting on it ends with
	// the test whatever the test does first, and no sleep outlives it.
	run := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestInterruptedRun$", "-test.count=1")
	run.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	run.Cancel = func() error { return syscall.Kill(-run.Process.Pid, syscall.SIGKILL) }
	run.Env = append(os.Environ(), interruptHelperDir+"="+dir, "LOAD_PROOF_BIN="+shell)
	var output bytes.Buffer
	run.Stdout = &output
	run.Stderr = &output
	require.NoError(t, run.Start())
	exited := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() { exited <- run.Wait() })
	t.Cleanup(wg.Wait)

	var binary int
	require.Eventually(t, func() bool {
		pid, err := os.ReadFile(filepath.Join(dir, startedMarker))
		if err != nil {
			return false
		}
		binary, err = strconv.Atoi(strings.TrimSpace(string(pid)))
		return err == nil
	}, helperStartDeadline, helperPollInterval, "the run's binary never started")
	require.NoError(t, run.Process.Signal(sig))

	timer := time.NewTimer(helperExitDeadline)
	defer timer.Stop()
	select {
	case err := <-exited:
		var exit *exec.ExitError
		require.ErrorAs(t, err, &exit, "the interrupted run exits failed")
		assert.Equal(t, 1, exit.ExitCode(), "the run failed its test rather than dying of the signal; output:\n%s", output.String())
	case <-timer.C:
		t.Fatalf("the interrupted run had not exited after %s: its binary was not killed", helperExitDeadline)
	}
	assert.FileExists(t, filepath.Join(dir, cleanedUpMarker), "the run's cleanups ran after the interrupt")
	assert.ErrorIs(t, syscall.Kill(binary, 0), syscall.ESRCH, "the binary that was running when the interrupt landed is gone")
}

// TestInterruptedRun is the run TestInterruptKillsTheBinaryAndRunsTheCleanups
// interrupts: it runs the binary under the proof's interruptible context,
// as TestLoadProof does, and records that its cleanup ran.
func TestInterruptedRun(t *testing.T) {
	dir := os.Getenv(interruptHelperDir)
	if dir == "" {
		t.Skip("run as a subprocess of TestInterruptKillsTheBinaryAndRunsTheCleanups")
	}
	ctx := interruptible(t)
	t.Cleanup(func() {
		assert.NoError(t, os.WriteFile(filepath.Join(dir, cleanedUpMarker), nil, 0o600))
	})
	// The stand-in writes the started marker itself, with its process ID,
	// and becomes the sleep, so the marker proves the binary is running.
	started := filepath.Join(dir, startedMarker)
	runBinary(t, ctx, config{Binary: os.Getenv("LOAD_PROOF_BIN")},
		"-c", `echo $$ > "$1" && exec sleep "$2"`, "stand-in", started, interruptedBinaryArg)
}
