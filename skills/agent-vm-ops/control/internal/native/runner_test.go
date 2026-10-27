package native

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type testWorkerRecorder struct {
	mu       sync.Mutex
	started  []WorkerIdentity
	finished []error
}

type blockingWorkerRecorder struct {
	testWorkerRecorder
	err error
}

func (recorder *testWorkerRecorder) WorkerStarted(_ context.Context, _ string, identity WorkerIdentity) error {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.started = append(recorder.started, identity)
	return nil
}

func (recorder *testWorkerRecorder) WorkerFinished(_ context.Context, _ string, _ WorkerIdentity, err error) error {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.finished = append(recorder.finished, err)
	return nil
}

func (recorder *blockingWorkerRecorder) WorkerStartAllowed(context.Context, string) error {
	return recorder.err
}

func TestLimitedBufferReportsTheWholeInputWhenTruncated(t *testing.T) {
	buffer := &limitedBuffer{limit: 3}
	input := []byte("abcdef")
	written, err := buffer.Write(input)
	if err != nil {
		t.Fatal(err)
	}
	if written != len(input) {
		t.Fatalf("Write returned %d, want %d", written, len(input))
	}
	if string(buffer.Bytes()) != "abc" || !buffer.Truncated {
		t.Fatalf("buffer = %q, truncated = %v", buffer.Bytes(), buffer.Truncated)
	}
}

func TestRunnerUsesTheProtocolOutputSplit(t *testing.T) {
	runner := NewRunner()
	if got := runner.stdoutLimit(); got != 2*1024*1024 {
		t.Fatalf("stdout limit = %d", got)
	}
	if got := runner.stderrLimit(); got != 64*1024 {
		t.Fatalf("stderr limit = %d", got)
	}
}

func TestCleanupTrackedPerformsFinalDiscovery(t *testing.T) {
	discoverCalls := 0
	presentCalls := 0
	if err := cleanupTrackedWith(42, cleanupOperations{
		discover: func(root int) error {
			if root != 42 {
				t.Fatalf("cleanup root = %d, want 42", root)
			}
			discoverCalls++
			return nil
		},
		kill: func() error { return nil },
		reap: func() error { return nil },
		present: func() (int, error) {
			presentCalls++
			if presentCalls == 1 {
				return 0, nil
			}
			if presentCalls == 2 {
				return 1, nil
			}
			return 0, nil
		},
		sleep: func(time.Duration) {},
	}); err != nil {
		t.Fatal(err)
	}
	if discoverCalls < 4 || presentCalls < 4 {
		t.Fatalf("cleanup scans = discover:%d present:%d, want a final rescan after the first empty observation", discoverCalls, presentCalls)
	}
}

func TestCleanupTrackedConfirmsProcessGroupAfterEmptyObservation(t *testing.T) {
	groupCalls := 0
	if err := cleanupTrackedWith(42, cleanupOperations{
		discover: func(int) error { return nil },
		kill:     func() error { return nil },
		reap:     func() error { return nil },
		present:  func() (int, error) { return 0, nil },
		groupAlive: func() (bool, error) {
			groupCalls++
			return groupCalls == 1, nil
		},
		sleep: func(time.Duration) {},
	}); err != nil {
		t.Fatal(err)
	}
	if groupCalls < 2 {
		t.Fatalf("group cleanup checks = %d, want a retry after the group remained", groupCalls)
	}
}

func TestCleanupTrackedFailsClosedOnProcessGroupObservationError(t *testing.T) {
	sentinel := errors.New("synthetic process-group observation failure")
	err := cleanupTrackedWith(42, cleanupOperations{
		discover:   func(int) error { return nil },
		kill:       func() error { return nil },
		reap:       func() error { return nil },
		present:    func() (int, error) { return 0, nil },
		groupAlive: func() (bool, error) { return false, sentinel },
		sleep:      func(time.Duration) {},
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("cleanupTrackedWith() error = %v, want %v", err, sentinel)
	}
}

func TestRunnerAllowsIndependentCommandsToOverlap(t *testing.T) {
	runner := NewRunner()
	started := time.Now()
	var group sync.WaitGroup
	errors := make(chan error, 2)
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := runner.Run(context.Background(), []string{"/bin/sh", "-c", "sleep 0.25"}, nil)
			errors <- err
		}()
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(started); elapsed > 450*time.Millisecond {
		t.Fatalf("independent native commands were serialized: %s", elapsed)
	}
}

func TestRunnerAppliesWorkerStartGuardBeforeSpawn(t *testing.T) {
	sentinel := errors.New("worker start blocked")
	marker := filepath.Join(t.TempDir(), "spawned")
	recorder := &blockingWorkerRecorder{err: sentinel}
	ctx := WithWorkerRecorder(context.Background(), "operation", recorder)
	_, err := NewRunner().Run(ctx, []string{"/bin/sh", "-c", "echo spawned > \"$1\"", "sh", marker}, nil)
	if !errors.Is(err, sentinel) {
		t.Fatalf("Run() error = %v, want %v", err, sentinel)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("worker start guard allowed command side effect: stat error = %v", statErr)
	}
}

func TestRunnerDurablyExposesWorkerIdentityToRecorder(t *testing.T) {
	recorder := &testWorkerRecorder{}
	ctx := WithWorkerRecorder(context.Background(), "operation", recorder)
	for range 50 {
		if _, err := NewRunner().Run(ctx, []string{"/bin/echo", "worker"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.started) != 50 || len(recorder.finished) != 50 {
		t.Fatalf("worker callbacks = %#v", recorder)
	}
	for index, identity := range recorder.started {
		if identity.PID <= 0 {
			t.Fatalf("worker identity[%d] = %#v", index, identity)
		}
		if runtime.GOOS == "linux" && identity.StartTime == "" {
			t.Fatalf("Linux worker identity[%d] lacks start time: %#v", index, identity)
		}
		if runtime.GOOS == "linux" && identity.ProcessGroup != identity.PID {
			t.Fatalf("Linux worker identity[%d] process group = %#v, want an isolated group", index, identity)
		}
		if recorder.finished[index] != nil {
			t.Fatalf("worker finish[%d] = %v", index, recorder.finished[index])
		}
	}
}

func TestCommandErrorRetainsBoundedDiagnostics(t *testing.T) {
	result := Result{Code: 17, Stdout: []byte("out"), Stderr: []byte("err"), StdoutTruncated: true}
	wrapped := wrapCommandError(result, errors.New("incus failed"))
	var commandErr *CommandError
	if !errors.As(wrapped, &commandErr) {
		t.Fatalf("wrapped error = %v", wrapped)
	}
	detail := commandErr.Diagnostic()
	if detail["exit_code"] != 17 || detail["stdout"] != "out" || detail["stderr"] != "err" || detail["stdout_truncated"] != true {
		t.Fatalf("diagnostic = %#v", detail)
	}
}

func TestTimeoutKillsTheWholeProcessGroupAndBoundsPipeCleanup(t *testing.T) {
	runner := NewRunner()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := runner.Run(ctx, []string{"/bin/sh", "-c", "sleep 30 & wait"}, nil)
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("Run error = %v, want a timeout", err)
	}
	if elapsed := time.Since(started); elapsed > 4*time.Second {
		t.Fatalf("timeout cleanup took %s", elapsed)
	}
}

func TestEscapedDescendantIsDetectedAndCleaned(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux process ownership boundary required")
	}
	tracker, err := newProcessTracker()
	if err != nil {
		t.Skipf("Linux process ownership boundary unavailable: %v", err)
	}
	delegated := tracker.hasOwnershipBoundary()
	tracker.Close()
	if !delegated {
		t.Skip("Linux cgroup delegation required for escaped descendant ownership")
	}
	pidPath := filepath.Join(t.TempDir(), "pid")
	runner := NewRunner()
	command := `setsid /bin/sh -c 'echo $$ > "$1"; sleep 30' sh "$1" &
while [ ! -s "$1" ]; do sleep 0.01; done
exit 0`
	_, err = runner.Run(context.Background(), []string{"/bin/sh", "-c", command, "sh", pidPath}, nil)
	if err == nil || !strings.Contains(err.Error(), "child processes") {
		t.Fatalf("Run error = %v, want escaped child failure", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	var lastPID int
	var lastStat []byte
	for time.Now().Before(deadline) {
		raw, readErr := os.ReadFile(pidPath)
		if readErr == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(raw)))
			if parseErr == nil {
				lastPID = pid
				lastStat, _ = os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
				if _, statErr := os.Stat("/proc/" + strconv.Itoa(pid)); os.IsNotExist(statErr) {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("escaped descendant still exists after runner cleanup: pid=%d stat=%q", lastPID, lastStat)
}
