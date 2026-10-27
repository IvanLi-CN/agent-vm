//go:build linux

package native

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
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
	command := exec.Command("/bin/sh", "-c", "sleep 30 & child=$!; echo $child; exit 0")
	configureCommand(command)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	tracker.Register(command.Process.Pid)
	childPID := 0
	defer func() {
		if tracker.rootGroup > 0 {
			_ = unix.Kill(-tracker.rootGroup, unix.SIGKILL)
		}
		if childPID > 0 {
			_ = unix.Kill(childPID, unix.SIGKILL)
		}
		_ = command.Wait()
		if childPID > 0 {
			_, _ = unix.Wait4(childPID, nil, 0, nil)
		}
		_ = tracker.Reap()
	}()
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		t.Fatal("worker did not report its background process")
	}
	childPID, err = strconv.Atoi(strings.TrimSpace(scanner.Text()))
	if err != nil {
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
	processGroup := command.Process.Pid
	if group, groupErr := unix.Getpgid(command.Process.Pid); groupErr == nil {
		processGroup = group
	}
	childPID := 0
	defer func() {
		_ = unix.Kill(-processGroup, unix.SIGKILL)
		if childPID > 0 {
			_ = unix.Kill(childPID, unix.SIGKILL)
		}
		_ = command.Wait()
		if childPID > 0 {
			_, _ = unix.Wait4(childPID, nil, 0, nil)
		}
	}()
	identity, err := workerIdentity(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		t.Fatal("worker did not report its background process")
	}
	childPID, err = strconv.Atoi(strings.TrimSpace(scanner.Text()))
	if err != nil {
		t.Fatal(err)
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

func TestKillWorkerReconcilesIdentityExitAfterPidfdOpen(t *testing.T) {
	identity := WorkerIdentity{PID: 123, StartTime: "1", ProcessGroup: 123}
	sentinel := errors.New("synthetic worker identity read failure")
	for _, test := range []struct {
		name      string
		readErr   error
		alive     bool
		aliveErr  error
		wantErr   string
		wantIsErr error
	}{
		{name: "group remains", readErr: &os.PathError{Op: "read", Path: "/proc/123/stat", Err: unix.ESRCH}, alive: true, wantErr: "process group remains"},
		{name: "group gone", readErr: &os.PathError{Op: "read", Path: "/proc/123/stat", Err: unix.ESRCH}},
		{name: "group observation fails", readErr: &os.PathError{Op: "read", Path: "/proc/123/stat", Err: unix.ESRCH}, aliveErr: sentinel, wantIsErr: sentinel},
		{name: "other identity error", readErr: sentinel, wantIsErr: sentinel},
	} {
		t.Run(test.name, func(t *testing.T) {
			closed := false
			sent := false
			ops := workerKillOps{
				openPIDFD: func(int) (int, error) { return 42, nil },
				closePIDFD: func(int) error {
					closed = true
					return nil
				},
				readIdentity: func(int) (WorkerIdentity, error) {
					return WorkerIdentity{}, test.readErr
				},
				workerAlive: func(WorkerIdentity) (bool, error) {
					return test.alive, test.aliveErr
				},
				sendSignal: func(int) error {
					sent = true
					return nil
				},
				killGroup: func(int) error {
					t.Fatal("killGroup called before worker identity was verified")
					return nil
				},
			}
			err := killWorkerWith(identity, ops)
			if test.wantIsErr != nil {
				if !errors.Is(err, test.wantIsErr) {
					t.Fatalf("killWorkerWith() error = %v, want %v", err, test.wantIsErr)
				}
			} else if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("killWorkerWith() error = %v, want %q", err, test.wantErr)
				}
			} else if err != nil {
				t.Fatalf("killWorkerWith() error = %v, want nil", err)
			}
			if !closed {
				t.Fatal("killWorkerWith() did not close the pidfd")
			}
			if sent {
				t.Fatal("killWorkerWith() signalled before worker identity was verified")
			}
		})
	}
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

func TestWorkerGroupAlivePropagatesProcessTableErrors(t *testing.T) {
	sentinel := errors.New("synthetic process-table failure")
	_, err := workerGroupAlive(123, func() (map[int]trackedProcess, error) {
		return nil, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("workerGroupAlive() error = %v, want %v", err, sentinel)
	}
}
