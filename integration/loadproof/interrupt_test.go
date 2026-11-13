package loadproof_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
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

// Markers the interrupted run writes: started just before it runs the
// binary, cleanedUp from its cleanup.
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
// SIGTERM from a process supervisor is — kills the binary, waits for it,
// and runs the test's cleanups, which is where the run's replication slot
// and seeded table are removed. The run is a subprocess of this test, so
// the signal reaches no other test, and its binary is a sleep that only a
// kill ends in time.
func TestInterruptKillsTheBinaryAndRunsTheCleanups(t *testing.T) {
	dir := t.TempDir()
	sleep, err := exec.LookPath("sleep")
	require.NoError(t, err)
	// The test's context ending kills the run, so the goroutine waiting on
	// it ends with the test whatever the test does first.
	run := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestInterruptedRun$", "-test.count=1")
	run.Env = append(os.Environ(), interruptHelperDir+"="+dir, "LOAD_PROOF_BIN="+sleep)
	var output bytes.Buffer
	run.Stdout = &output
	run.Stderr = &output
	require.NoError(t, run.Start())
	exited := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() { exited <- run.Wait() })
	t.Cleanup(wg.Wait)

	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(dir, startedMarker))
		return err == nil
	}, helperStartDeadline, helperPollInterval, "the run never reached its binary")
	require.NoError(t, run.Process.Signal(os.Interrupt))

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
	require.NoError(t, os.WriteFile(filepath.Join(dir, startedMarker), nil, 0o600))
	runBinary(t, ctx, config{Binary: os.Getenv("LOAD_PROOF_BIN")}, interruptedBinaryArg)
}
