//go:build linux

package native

import (
	"bufio"
	"errors"
	"fmt"
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
	command := exec.Command("/bin/sh", "-c", "sleep 30 & child=$!; echo $child; read _; exit 0")
	configureCommand(command)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	defer stdout.Close()
	tracker.Register(command.Process.Pid)
	childPID := 0
	childStart := ""
	cleaned := false
	defer func() {
		if !cleaned {
			_ = tracker.Kill()
			_ = signalTestProcess(command.Process.Pid, tracker.rootStart)
			if childPID > 0 {
				_ = signalTestProcess(childPID, childStart)
			}
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
	if child, err := workerIdentity(childPID); err == nil {
		childStart = child.StartTime
	}
	if err := tracker.Discover(command.Process.Pid); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	if err := stdin.Close(); err != nil {
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
	cleaned = true
}

func TestProcessTrackerRejectsUnobservedReusedProcessGroup(t *testing.T) {
	tracker := &processTracker{
		root:      100,
		rootStart: "root-start",
		rootGroup: 100,
		baseline:  make(map[int]trackedProcess),
		tracked:   make(map[int]trackedProcess),
	}
	first := map[int]trackedProcess{
		100: {pid: 100, start: "root-start", parent: 1, group: 100, state: 'S'},
		200: {pid: 200, start: "old-child", parent: 100, group: 100, state: 'S'},
	}
	second := map[int]trackedProcess{
		300: {pid: 300, start: "new-child", parent: 1, group: 100, state: 'S'},
	}
	snapshot := 0
	readTable := func() (map[int]trackedProcess, error) {
		snapshot++
		if snapshot == 1 {
			return first, nil
		}
		return second, nil
	}
	readRoot := func(pid int) (trackedProcess, error) {
		return first[pid], nil
	}
	if err := tracker.discoverWithRead(100, readTable, readRoot); err != nil {
		t.Fatalf("initial discover() error = %v", err)
	}
	if err := tracker.discoverWithRead(100, readTable, readRoot); err == nil || !strings.Contains(err.Error(), "group membership unavailable") {
		t.Fatalf("reused group discover() error = %v, want group membership failure", err)
	}
}

func TestProcessTrackerRechecksRootBeforeGroupClaim(t *testing.T) {
	tracker := &processTracker{
		root:      100,
		rootStart: "old-root",
		rootGroup: 100,
		baseline:  make(map[int]trackedProcess),
		tracked:   make(map[int]trackedProcess),
	}
	table := map[int]trackedProcess{
		100: {pid: 100, start: "old-root", parent: 1, group: 100, state: 'S'},
		200: {pid: 200, start: "new-child", parent: 1, group: 100, state: 'S'},
	}
	readTable := func() (map[int]trackedProcess, error) { return table, nil }
	readRoot := func(int) (trackedProcess, error) {
		return trackedProcess{pid: 100, start: "new-root", parent: 1, group: 100, state: 'S'}, nil
	}
	if err := tracker.discoverWithRead(100, readTable, readRoot); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("inconsistent root discover() error = %v, want identity failure", err)
	}
}

func TestProcessTrackerAcceptsReapedRootWithEmptyGroup(t *testing.T) {
	group := 1 << 30
	tracker := &processTracker{
		root:         100,
		rootStart:    "root-start",
		rootGroup:    group,
		baseline:     make(map[int]trackedProcess),
		tracked:      make(map[int]trackedProcess),
		groupMembers: make(map[int]trackedProcess),
	}
	first := map[int]trackedProcess{
		100: {pid: 100, start: "root-start", parent: 1, group: group, state: 'S'},
	}
	empty := map[int]trackedProcess{}
	snapshot := 0
	readTable := func() (map[int]trackedProcess, error) {
		snapshot++
		if snapshot == 1 {
			return first, nil
		}
		return empty, nil
	}
	readRoot := func(int) (trackedProcess, error) {
		return trackedProcess{}, &os.PathError{Op: "read", Path: "/proc/100/stat", Err: unix.ESRCH}
	}
	if err := tracker.discoverWithRead(100, readTable, readRoot); err != nil {
		t.Fatalf("reaped root discover() error = %v", err)
	}
}

func TestProcessTrackerRetainsObservedDescendantAfterRootReparent(t *testing.T) {
	group := 1 << 30
	tracker := &processTracker{
		root:         100,
		rootStart:    "root-start",
		rootGroup:    group,
		baseline:     make(map[int]trackedProcess),
		tracked:      make(map[int]trackedProcess),
		groupMembers: make(map[int]trackedProcess),
	}
	first := map[int]trackedProcess{
		100: {pid: 100, start: "root-start", parent: 1, group: group, state: 'S'},
		200: {pid: 200, start: "detached-child", parent: 100, group: 200, state: 'S'},
	}
	refreshed := map[int]trackedProcess{
		200: {pid: 200, start: "detached-child", parent: 1, group: 200, state: 'S'},
		201: {pid: 201, start: "detached-grandchild", parent: 200, group: 201, state: 'S'},
	}
	empty := map[int]trackedProcess{}
	snapshot := 0
	readTable := func() (map[int]trackedProcess, error) {
		snapshot++
		switch snapshot {
		case 1:
			return first, nil
		case 2:
			return refreshed, nil
		default:
			return empty, nil
		}
	}
	readRoot := func(int) (trackedProcess, error) {
		return trackedProcess{}, &os.PathError{Op: "read", Path: "/proc/100/stat", Err: unix.ESRCH}
	}
	if err := tracker.discoverWithRead(100, readTable, readRoot); err != nil {
		t.Fatalf("reparented descendant discover() error = %v", err)
	}
	if item, ok := tracker.tracked[200]; !ok || item.start != "detached-child" {
		t.Fatalf("tracked detached descendant = %#v, want observed identity", tracker.tracked)
	}
	if item, ok := tracker.tracked[201]; !ok || item.start != "detached-grandchild" {
		t.Fatalf("tracked reparented descendant = %#v, want current descendant identity", tracker.tracked)
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
	childPID := 0
	childStart := ""
	identity := WorkerIdentity{}
	defer func() {
		_ = signalTestProcess(identity.PID, identity.StartTime)
		if childPID > 0 {
			_ = signalTestProcess(childPID, childStart)
		}
		_ = command.Wait()
		if childPID > 0 {
			_, _ = unix.Wait4(childPID, nil, 0, nil)
		}
	}()
	identity, err = workerIdentity(command.Process.Pid)
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
	if child, err := workerIdentity(childPID); err == nil {
		childStart = child.StartTime
	}
	if err := signalTestProcess(identity.PID, identity.StartTime); err != nil {
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
	if err := signalTestProcess(childPID, childStart); err != nil && !processGone(err) {
		t.Fatal(err)
	}
	if _, err := unix.Wait4(childPID, nil, 0, nil); err != nil && !errors.Is(err, unix.ECHILD) {
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

func TestKillWorkerCleansRootAndLiveGroupMember(t *testing.T) {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Skipf("Linux child subreaper unavailable: %v", err)
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
	waited := false
	defer func() {
		if !waited {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	identity, err := workerIdentity(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		t.Fatal("worker did not report its live group member")
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(scanner.Text()))
	if err != nil {
		t.Fatal(err)
	}
	childIdentity, err := workerIdentity(childPID)
	if err != nil {
		t.Fatal(err)
	}
	if childIdentity.ProcessGroup != identity.ProcessGroup {
		t.Fatalf("child process group = %d, want root group %d", childIdentity.ProcessGroup, identity.ProcessGroup)
	}
	if err := KillWorker(identity); err != nil {
		t.Fatalf("KillWorker() error = %v, want nil", err)
	}
	_ = command.Wait()
	waited = true
	alive, err := WorkerAlive(identity)
	if err != nil {
		t.Fatal(err)
	}
	if alive {
		t.Fatal("KillWorker() left the process group alive")
	}
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
				killGroup: func(WorkerIdentity, *workerGroupCleanup) error {
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

func TestKillWorkerAnchorsGroupBeforeRootSignal(t *testing.T) {
	identity := WorkerIdentity{PID: 123, StartTime: "1", ProcessGroup: 123}
	events := make([]string, 0, 2)
	err := killWorkerWith(identity, workerKillOps{
		openPIDFD:  func(int) (int, error) { return 42, nil },
		closePIDFD: func(int) error { return nil },
		readIdentity: func(int) (WorkerIdentity, error) {
			return identity, nil
		},
		workerAlive: func(WorkerIdentity) (bool, error) { return false, nil },
		sendSignal: func(int) error {
			events = append(events, "root")
			return nil
		},
		killGroup: func(WorkerIdentity, *workerGroupCleanup) error {
			events = append(events, "group")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("killWorkerWith() error = %v, want nil", err)
	}
	if strings.Join(events, ",") != "group,root" {
		t.Fatalf("kill order = %v, want group,root", events)
	}
}

func TestKillProcessGroupDoesNotClaimUnanchoredMembers(t *testing.T) {
	identity := WorkerIdentity{PID: 123, StartTime: "root", ProcessGroup: 123}
	cleanup := &workerGroupCleanup{rootPIDFD: 42, members: make(map[int]trackedProcess)}
	err := killProcessGroupWith(identity, cleanup, processGroupKillOps{
		readTable: func() (map[int]trackedProcess, error) {
			return map[int]trackedProcess{
				456: {pid: 456, start: "unanchored", parent: 1, group: 123, state: 'S'},
			}, nil
		},
		openPIDFD: func(int) (int, error) {
			t.Fatal("root-less first scan opened an unanchored member pidfd")
			return -1, nil
		},
		closePIDFD: func(int) error { return nil },
		readProcess: func(int) (trackedProcess, error) {
			t.Fatal("root-less first scan re-read an unanchored member")
			return trackedProcess{}, nil
		},
		sendSignal: func(int) error {
			t.Fatal("root-less first scan signalled an unanchored member")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("killProcessGroupWith() error = %v, want nil", err)
	}
	if cleanup.rootSignaled || len(cleanup.members) != 0 {
		t.Fatalf("root-less cleanup state = %#v, want no claimed members or root signal", cleanup)
	}
}

func TestKillProcessGroupRejectsStaleRootSnapshot(t *testing.T) {
	identity := WorkerIdentity{PID: 123, StartTime: "root", ProcessGroup: 123}
	for _, test := range []struct {
		name      string
		readRoot  func(int) (trackedProcess, error)
		wantErr   string
		wantNoErr bool
	}{
		{
			name: "root exited after table scan",
			readRoot: func(int) (trackedProcess, error) {
				return trackedProcess{}, &os.PathError{Op: "read", Path: "/proc/123/stat", Err: unix.ESRCH}
			},
			wantNoErr: true,
		},
		{
			name: "root identity changed after table scan",
			readRoot: func(int) (trackedProcess, error) {
				return trackedProcess{pid: 123, start: "new-root", group: 123}, nil
			},
			wantErr: "identity changed",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			cleanup := &workerGroupCleanup{rootPIDFD: 42, members: make(map[int]trackedProcess)}
			err := killProcessGroupWith(identity, cleanup, processGroupKillOps{
				readTable: func() (map[int]trackedProcess, error) {
					return map[int]trackedProcess{
						123: {pid: 123, start: "root", parent: 1, group: 123, state: 'S'},
						456: {pid: 456, start: "member", parent: 123, group: 123, state: 'S'},
					}, nil
				},
				openPIDFD: func(int) (int, error) {
					t.Fatal("stale root snapshot claimed a member pidfd")
					return -1, nil
				},
				closePIDFD:  func(int) error { return nil },
				readProcess: test.readRoot,
				sendSignal: func(int) error {
					t.Fatal("stale root snapshot signalled a process")
					return nil
				},
			})
			if test.wantNoErr {
				if err != nil {
					t.Fatalf("killProcessGroupWith() error = %v, want nil", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("killProcessGroupWith() error = %v, want %q", err, test.wantErr)
			}
			if len(cleanup.members) != 0 || cleanup.rootSignaled {
				t.Fatalf("stale root cleanup state = %#v, want no claimed members or root signal", cleanup)
			}
		})
	}
}

func TestReapWorkerGroupUsesVerifiedIdentities(t *testing.T) {
	identity := WorkerIdentity{PID: 100, StartTime: "root", ProcessGroup: 100}
	cleanup := &workerGroupCleanup{
		members: map[int]trackedProcess{
			200: {pid: 200, start: "member", group: 100},
		},
	}
	waited := make(map[int]bool)
	err := reapWorkerGroupWith(identity, cleanup, processReapOps{
		readProcess: func(pid int) (trackedProcess, error) {
			switch pid {
			case 100:
				return trackedProcess{pid: 100, start: "root", parent: 999, group: 100, state: 'Z'}, nil
			case 200:
				return trackedProcess{pid: 200, start: "member", parent: 999, group: 100, state: 'Z'}, nil
			default:
				t.Fatalf("reaper inspected unverified pid %d", pid)
				return trackedProcess{}, nil
			}
		},
		wait: func(pid int) error {
			waited[pid] = true
			return nil
		},
		owner: 999,
	})
	if err != nil {
		t.Fatalf("reapWorkerGroupWith() error = %v, want nil", err)
	}
	if len(waited) != 2 || !waited[100] || !waited[200] {
		t.Fatalf("reaped identities = %#v, want root and verified member", waited)
	}
}

func TestReapWorkerGroupRejectsReusedMemberIdentity(t *testing.T) {
	identity := WorkerIdentity{PID: 100, StartTime: "root", ProcessGroup: 100}
	cleanup := &workerGroupCleanup{members: map[int]trackedProcess{
		200: {pid: 200, start: "old-member", group: 100},
	}}
	waited := false
	err := reapWorkerGroupWith(identity, cleanup, processReapOps{
		readProcess: func(pid int) (trackedProcess, error) {
			if pid == identity.PID {
				return trackedProcess{pid: pid, start: identity.StartTime, group: identity.ProcessGroup, state: 'S'}, nil
			}
			return trackedProcess{pid: pid, start: "new-member", parent: 999, group: identity.ProcessGroup, state: 'Z'}, nil
		},
		wait: func(int) error {
			waited = true
			return nil
		},
		owner: 999,
	})
	if err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("reapWorkerGroupWith() error = %v, want identity-change failure", err)
	}
	if waited {
		t.Fatal("reaper waited for a reused process identity")
	}
}

func TestKillWorkerConfirmsProcessGroupGoneAfterMemberScan(t *testing.T) {
	identity := WorkerIdentity{PID: 123, StartTime: "1", ProcessGroup: 123}
	groupChecks := 0
	killGroupCalls := 0
	reapGroupCalls := 0
	err := killWorkerWith(identity, workerKillOps{
		openPIDFD:  func(int) (int, error) { return 42, nil },
		closePIDFD: func(int) error { return nil },
		readIdentity: func(int) (WorkerIdentity, error) {
			return identity, nil
		},
		workerAlive: func(WorkerIdentity) (bool, error) { return true, nil },
		sendSignal:  func(int) error { return nil },
		killGroup: func(WorkerIdentity, *workerGroupCleanup) error {
			killGroupCalls++
			return nil
		},
		reapGroup: func(WorkerIdentity, *workerGroupCleanup) error {
			reapGroupCalls++
			return nil
		},
		groupAlive: func(int) (bool, error) {
			groupChecks++
			return groupChecks == 1, nil
		},
		sleep: func(time.Duration) {},
	})
	if err != nil {
		t.Fatalf("killWorkerWith() error = %v, want nil", err)
	}
	if killGroupCalls != 2 || reapGroupCalls != 2 || groupChecks != 2 {
		t.Fatalf("group cleanup calls = kill:%d reap:%d check:%d, want 2 each", killGroupCalls, reapGroupCalls, groupChecks)
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

func TestWorkerGroupAliveUsesKernelGroupPresence(t *testing.T) {
	command := exec.Command("sleep", "30")
	configureCommand(command)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	}()
	group, err := unix.Getpgid(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	alive, err := workerGroupAlive(group, func() (map[int]trackedProcess, error) {
		return map[int]trackedProcess{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !alive {
		t.Fatal("workerGroupAlive() missed a process group hidden by the synthetic empty snapshot")
	}
}

func TestWorkerGroupAliveTreatsOnlyZombiesAsGone(t *testing.T) {
	group := 1 << 30
	calls := 0
	alive, err := workerGroupAlive(group, func() (map[int]trackedProcess, error) {
		calls++
		return map[int]trackedProcess{456: {pid: 456, group: group, state: 'Z'}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if alive {
		t.Fatal("workerGroupAlive() treated a zombie-only group as live")
	}
	if calls != 2 {
		t.Fatalf("workerGroupAlive() read table %d times, want zombie refresh", calls)
	}
}

func TestWorkerGroupAliveRefreshesZombieSnapshotBeforeKernelProbe(t *testing.T) {
	command := exec.Command("sleep", "30")
	configureCommand(command)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	}()
	group, err := unix.Getpgid(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	alive, err := workerGroupAlive(group, func() (map[int]trackedProcess, error) {
		calls++
		if calls == 1 {
			return map[int]trackedProcess{456: {pid: 456, group: group, state: 'Z'}}, nil
		}
		return map[int]trackedProcess{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !alive {
		t.Fatal("workerGroupAlive() missed a live kernel process group after a zombie snapshot")
	}
	if calls != 2 {
		t.Fatalf("workerGroupAlive() read table %d times, want zombie refresh", calls)
	}
}

func signalTestProcess(pid int, start string) error {
	if pid <= 0 || start == "" {
		return nil
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	current, err := workerIdentity(pid)
	if err != nil {
		return err
	}
	if current.StartTime != start {
		return fmt.Errorf("test process identity changed for pid %d", pid)
	}
	return unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
}
