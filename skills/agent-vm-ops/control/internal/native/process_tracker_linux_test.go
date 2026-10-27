//go:build linux

package native

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestProcessGoneRecognizesProcfsExitErrors(t *testing.T) {
	for name, err := range map[string]error{
		"missing path": os.ErrNotExist,
		"procfs exit":  unix.ESRCH,
	} {
		t.Run(name, func(t *testing.T) {
			if !processGone(err) {
				t.Fatalf("processGone(%v) = false", err)
			}
		})
	}
	if processGone(errors.New("unrelated process error")) {
		t.Fatal("processGone() accepted an unrelated error")
	}
}

func TestReadProcessTableSkipsProcfsExit(t *testing.T) {
	injected := false
	processes, err := readProcessTableWith(func(path string) ([]byte, error) {
		if !injected {
			injected = true
			return nil, &os.PathError{Op: "read", Path: path, Err: unix.ESRCH}
		}
		return os.ReadFile(path)
	})
	if err != nil {
		t.Fatalf("readProcessTableWith() error = %v", err)
	}
	if !injected {
		t.Fatal("readProcessTableWith() did not exercise the injected procfs read")
	}
	if len(processes) == 0 {
		t.Fatal("readProcessTableWith() returned no processes after skipping the exited process")
	}
}

func TestReadProcessTablePropagatesProcfsReadErrors(t *testing.T) {
	sentinel := errors.New("synthetic procfs read failure")
	_, err := readProcessTableWith(func(string) ([]byte, error) {
		return nil, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("readProcessTableWith() error = %v, want %v", err, sentinel)
	}
}

func TestReadProcessTableAllowsKernelThreads(t *testing.T) {
	processes, err := readProcessTable()
	if err != nil {
		t.Fatalf("readProcessTable() error = %v", err)
	}
	if len(processes) == 0 {
		t.Fatal("readProcessTable() returned no processes")
	}
}

func TestProcessTrackerFindsReparentedGroupMemberWithoutCgroup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux process group tracking required")
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Skipf("Linux child subreaper unavailable: %v", err)
	}
	tracker := &processTracker{
		owner:    os.Getpid(),
		baseline: make(map[int]trackedProcess),
		tracked:  make(map[int]trackedProcess),
		cgroupFD: -1,
	}
	command := exec.Command("/bin/sh", "-c", "sleep 30 & exit 0")
	configureCommand(command)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	tracker.Register(command.Process.Pid)
	defer func() {
		if tracker.rootGroup > 0 {
			_ = unix.Kill(-tracker.rootGroup, unix.SIGKILL)
		}
		_ = command.Wait()
		_ = tracker.Reap()
	}()
	if err := tracker.Discover(command.Process.Pid); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("command.Wait() = %v", err)
	}
	if err := tracker.Discover(command.Process.Pid); err != nil {
		t.Fatal(err)
	}
	live, err := tracker.Live()
	if err != nil {
		t.Fatal(err)
	}
	if live == 0 {
		t.Fatal("tracker missed the live reparented process-group member")
	}
	if err := cleanupTracked(tracker, command.Process.Pid); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerAliveDetectsSurvivingProcessGroupAfterRootExit(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux worker identity required")
	}
	command := exec.Command("/bin/sh", "-c", "sleep 30 & child=$!; echo $child; wait")
	configureCommand(command)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	identity, err := workerIdentity(command.Process.Pid)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal(err)
	}
	defer func() {
		_ = unix.Kill(-identity.ProcessGroup, unix.SIGKILL)
		_ = command.Wait()
	}()
	if !bufio.NewScanner(stdout).Scan() {
		t.Fatal("worker did not report its background process")
	}
	if err := unix.Kill(identity.PID, unix.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	alive, err := WorkerAlive(identity)
	if err != nil {
		t.Fatal(err)
	}
	if !alive {
		t.Fatal("WorkerAlive() missed the surviving process-group member")
	}
	if err := KillWorker(identity); err == nil || !strings.Contains(err.Error(), "process group remains") {
		t.Fatalf("KillWorker() error = %v, want process-group safety error", err)
	}
	if err := unix.Kill(-identity.ProcessGroup, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		alive, err = WorkerAlive(identity)
		if err != nil {
			t.Fatal(err)
		}
		if !alive {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("process group remained alive after cleanup")
}

func TestWorkerAliveRejectsMissingProcessGroup(t *testing.T) {
	identity, err := workerIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	identity.ProcessGroup = 0
	if _, err := WorkerAlive(identity); err == nil {
		t.Fatal("WorkerAlive() accepted an identity without a process group")
	}
}
