//go:build linux

package native

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

type trackedProcess struct {
	pid    int
	start  string
	parent int
	group  int
	state  byte
}

type processTracker struct {
	owner      int
	root       int
	rootGroup  int
	rootStart  string
	baseline   map[int]trackedProcess
	tracked    map[int]trackedProcess
	cgroupPath string
	cgroupFD   int
}

var nativeProcessTreeMu sync.Mutex

func (tracker *processTracker) Identity(root int) (WorkerIdentity, error) {
	if root <= 0 {
		return WorkerIdentity{}, fmt.Errorf("invalid native process pid %d", root)
	}
	return workerIdentity(root)
}

func (tracker *processTracker) Register(root int) {
	tracker.root = root
	// Commands are started with Setpgid, so the PID is the expected process
	// group even if the root exits before procfs identity can be observed.
	tracker.rootGroup = root
	if process, err := readProcess(root); err == nil {
		tracker.rootStart = process.start
		tracker.rootGroup = process.group
	}
}

func startTrackedCommand(command *exec.Cmd, tracker *processTracker) (bool, error) {
	nativeProcessTreeMu.Lock()
	defer nativeProcessTreeMu.Unlock()
	if err := command.Start(); err != nil {
		return false, err
	}
	if tracker.cgroupFD >= 0 {
		_ = unix.Close(tracker.cgroupFD)
		tracker.cgroupFD = -1
	}
	tracker.Register(command.Process.Pid)
	return true, tracker.discover(command.Process.Pid)
}

func newProcessTracker() (*processTracker, error) {
	owner := os.Getpid()
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return nil, fmt.Errorf("enable child subreaper: %w", err)
	}
	baseline, err := readProcessTree(owner)
	if err != nil {
		return nil, fmt.Errorf("snapshot native process tree: %w", err)
	}
	tracker := &processTracker{owner: owner, baseline: baseline, tracked: make(map[int]trackedProcess), cgroupFD: -1}
	tracker.prepareCgroup()
	return tracker, nil
}

func (tracker *processTracker) prepareCgroup() {
	content, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return
	}
	var relative string
	for _, line := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(line, "0::") {
			relative = strings.TrimSpace(strings.TrimPrefix(line, "0::"))
			break
		}
	}
	if relative == "" || !strings.HasPrefix(relative, "/") {
		return
	}
	parent := filepath.Join("/sys/fs/cgroup", filepath.FromSlash(relative))
	path, err := os.MkdirTemp(parent, "agent-vm-native-")
	if err != nil {
		return
	}
	fd, err := unix.Open(path, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = os.Remove(path)
		return
	}
	tracker.cgroupPath = path
	tracker.cgroupFD = fd
}

func (tracker *processTracker) configureCommand(command *exec.Cmd) {
	if tracker.cgroupFD < 0 {
		return
	}
	attributes := command.SysProcAttr
	if attributes == nil {
		attributes = &syscall.SysProcAttr{}
	}
	attributes.UseCgroupFD = true
	attributes.CgroupFD = tracker.cgroupFD
	command.SysProcAttr = attributes
}

func (tracker *processTracker) hasOwnershipBoundary() bool {
	return tracker.cgroupPath != ""
}

func (tracker *processTracker) Discover(root int) error {
	nativeProcessTreeMu.Lock()
	defer nativeProcessTreeMu.Unlock()
	return tracker.discover(root)
}

func (tracker *processTracker) discover(root int) error {
	if tracker.root == 0 {
		tracker.root = root
	}
	all, err := readProcessTable()
	if err != nil {
		return fmt.Errorf("inspect native process tree: %w", err)
	}
	if rootProcess, present := all[root]; present {
		if tracker.rootStart == "" {
			tracker.rootStart = rootProcess.start
		}
		if tracker.rootStart != rootProcess.start {
			return fmt.Errorf("native process identity changed for pid %d", root)
		}
		if tracker.rootGroup != rootProcess.group {
			return fmt.Errorf("native process group changed for pid %d", root)
		}
	}
	related := descendants(all, root)
	cgroupProcesses, err := tracker.cgroupProcesses()
	if err != nil {
		return fmt.Errorf("inspect native cgroup: %w", err)
	}
	for pid, item := range cgroupProcesses {
		related[pid] = item
	}
	// Without a delegated cgroup, a descendant can outlive the root and be
	// reparented to the subreaper before the next process-table snapshot. The
	// isolated process group remains the ownership boundary in that case.
	if !tracker.hasOwnershipBoundary() && tracker.rootGroup > 0 {
		groupMembers := make(map[int]trackedProcess)
		for pid, item := range all {
			if item.group == tracker.rootGroup {
				groupMembers[pid] = item
			}
		}
		if _, rootPresent := all[root]; !rootPresent && tracker.rootStart == "" && len(groupMembers) > 0 {
			return errors.New("native process root identity unavailable while its process group remains")
		}
		for pid, item := range groupMembers {
			if pid == root && tracker.rootStart != "" && item.start != tracker.rootStart {
				return fmt.Errorf("native process identity changed for pid %d", root)
			}
			related[pid] = item
		}
	}
	// A detached descendant is only owned after this tracker has observed it
	// while it was attached to the command tree. Do not scan every child of the
	// subreaper: a concurrent command may have reparented descendants there.
	for pid := range tracker.tracked {
		if _, present := all[pid]; present {
			for child, item := range descendants(all, pid) {
				related[child] = item
			}
		}
	}
	for pid, item := range related {
		if baseline, ok := tracker.baseline[pid]; ok {
			if baseline.start != item.start {
				return fmt.Errorf("process identity changed for pid %d", pid)
			}
			continue
		}
		if previous, ok := tracker.tracked[pid]; ok && previous.start != item.start {
			return fmt.Errorf("tracked process identity changed for pid %d", pid)
		}
		tracker.tracked[pid] = item
	}
	return nil
}

func (tracker *processTracker) cgroupProcesses() (map[int]trackedProcess, error) {
	if tracker.cgroupPath == "" {
		return nil, nil
	}
	content, err := os.ReadFile(filepath.Join(tracker.cgroupPath, "cgroup.procs"))
	if err != nil {
		return nil, err
	}
	processes := make(map[int]trackedProcess)
	for _, line := range strings.Split(string(content), "\n") {
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || pid <= 0 {
			continue
		}
		process, err := readProcess(pid)
		if processGone(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read process %d: %w", pid, err)
		}
		processes[pid] = process
	}
	return processes, nil
}

func (tracker *processTracker) Live() (int, error) {
	live := 0
	for pid, item := range tracker.tracked {
		current, err := readProcess(pid)
		if processGone(err) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if current.start != item.start {
			return 0, fmt.Errorf("tracked process identity changed for pid %d", pid)
		}
		if current.state != 'Z' {
			live++
		}
	}
	return live, nil
}

func (tracker *processTracker) Present() (int, error) {
	present := 0
	for pid, item := range tracker.tracked {
		current, err := readProcess(pid)
		if processGone(err) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if current.start != item.start {
			return 0, fmt.Errorf("tracked process identity changed for pid %d", pid)
		}
		present++
	}
	return present, nil
}

func (tracker *processTracker) Kill() error {
	var first error
	for pid, item := range tracker.tracked {
		// Open the pidfd before inspecting /proc again. The pidfd pins the
		// process identity; a PID reuse between read and open must not be
		// signalled.
		fd, openErr := unix.PidfdOpen(pid, 0)
		if openErr != nil {
			if errors.Is(openErr, unix.ESRCH) {
				continue
			}
			if first == nil {
				first = fmt.Errorf("open pidfd for %d: %w", pid, openErr)
			}
			continue
		}
		current, err := readProcess(pid)
		if processGone(err) {
			_ = unix.Close(fd)
			continue
		}
		if err != nil {
			_ = unix.Close(fd)
			if first == nil {
				first = err
			}
			continue
		}
		if current.start != item.start {
			_ = unix.Close(fd)
			if first == nil {
				first = fmt.Errorf("tracked process identity changed for pid %d", pid)
			}
			continue
		}
		if current.state == 'Z' {
			_ = unix.Close(fd)
			continue
		}
		signalErr := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
		_ = unix.Close(fd)
		if signalErr != nil && !errors.Is(signalErr, unix.ESRCH) && first == nil {
			first = fmt.Errorf("kill tracked process %d: %w", pid, signalErr)
		}
	}
	return first
}

func (tracker *processTracker) Reap() error {
	var first error
	for pid, item := range tracker.tracked {
		if pid == tracker.root {
			// exec.Cmd.Wait owns the command root. Reaping it here races with
			// Wait and can make a successful command look like an ECHILD.
			continue
		}
		current, err := readProcess(pid)
		if processGone(err) {
			continue
		}
		if err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		if current.start != item.start {
			if first == nil {
				first = fmt.Errorf("tracked process identity changed for pid %d", pid)
			}
			continue
		}
		if current.parent != tracker.owner {
			continue
		}
		if _, waitErr := unix.Wait4(pid, nil, unix.WNOHANG, nil); waitErr != nil && !errors.Is(waitErr, unix.ECHILD) && first == nil {
			first = fmt.Errorf("reap tracked process %d: %w", pid, waitErr)
		}
	}
	return first
}

func (tracker *processTracker) Close() {
	if tracker.cgroupFD >= 0 {
		_ = unix.Close(tracker.cgroupFD)
		tracker.cgroupFD = -1
	}
	if tracker.cgroupPath != "" {
		_ = os.Remove(tracker.cgroupPath)
		tracker.cgroupPath = ""
	}
}

func readProcessTree(root int) (map[int]trackedProcess, error) {
	all, err := readProcessTable()
	if err != nil {
		return nil, err
	}
	return descendants(all, root), nil
}

func readProcessTable() (map[int]trackedProcess, error) {
	return readProcessTableWith(os.ReadFile)
}

func readProcessTableWith(readFile func(string) ([]byte, error)) (map[int]trackedProcess, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	result := make(map[int]trackedProcess)
	for _, entry := range entries {
		pid, parseErr := strconv.Atoi(entry.Name())
		if parseErr != nil || pid <= 0 {
			continue
		}
		item, statErr := readProcessWith(readFile, pid)
		if processGone(statErr) {
			continue
		}
		if statErr != nil {
			return nil, statErr
		}
		result[pid] = item
	}
	return result, nil
}

func processGone(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH)
}

func readProcess(pid int) (trackedProcess, error) {
	return readProcessWith(os.ReadFile, pid)
}

func readProcessWith(readFile func(string) ([]byte, error), pid int) (trackedProcess, error) {
	raw, err := readFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return trackedProcess{}, err
	}
	end := strings.LastIndexByte(string(raw), ')')
	if end < 0 || end+2 >= len(raw) {
		return trackedProcess{}, fmt.Errorf("invalid /proc/%d/stat", pid)
	}
	fields := strings.Fields(string(raw[end+2:]))
	if len(fields) < 20 || len(fields[0]) != 1 {
		return trackedProcess{}, fmt.Errorf("invalid /proc/%d/stat fields", pid)
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		return trackedProcess{}, fmt.Errorf("invalid parent for pid %d: %w", pid, err)
	}
	group, err := strconv.Atoi(fields[2])
	if err != nil {
		return trackedProcess{}, fmt.Errorf("invalid process group for pid %d", pid)
	}
	return trackedProcess{pid: pid, start: fields[19], parent: parent, group: group, state: fields[0][0]}, nil
}

func descendants(all map[int]trackedProcess, root int) map[int]trackedProcess {
	result := make(map[int]trackedProcess)
	if item, ok := all[root]; ok {
		result[root] = item
	}
	changed := true
	for changed {
		changed = false
		for pid, item := range all {
			if _, exists := result[pid]; exists {
				continue
			}
			if _, parentExists := result[item.parent]; parentExists {
				result[pid] = item
				changed = true
			}
		}
	}
	return result
}
