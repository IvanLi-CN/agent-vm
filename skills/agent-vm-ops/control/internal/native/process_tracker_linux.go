//go:build linux

package native

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

type trackedProcess struct {
	pid    int
	start  string
	parent int
	state  byte
}

type processTracker struct {
	owner    int
	root     int
	baseline map[int]trackedProcess
	tracked  map[int]trackedProcess
}

var activeNativeRoots sync.Map

func registerNativeRoot(pid int)   { activeNativeRoots.Store(pid, struct{}{}) }
func unregisterNativeRoot(pid int) { activeNativeRoots.Delete(pid) }

func underOtherNativeRoot(all map[int]trackedProcess, pid, ownRoot int) bool {
	seen := map[int]struct{}{}
	for current := pid; ; {
		if current != ownRoot {
			if _, active := activeNativeRoots.Load(current); active {
				return true
			}
		}
		if _, exists := seen[current]; exists {
			return false
		}
		seen[current] = struct{}{}
		item, exists := all[current]
		if !exists || item.parent <= 0 || item.parent == current {
			return false
		}
		current = item.parent
	}
}

func (tracker *processTracker) Identity(root int) (WorkerIdentity, error) {
	if root <= 0 {
		return WorkerIdentity{}, fmt.Errorf("invalid native process pid %d", root)
	}
	return workerIdentity(root)
}

func (tracker *processTracker) Register(root int) {
	tracker.root = root
	registerNativeRoot(root)
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
	return &processTracker{owner: owner, baseline: baseline, tracked: make(map[int]trackedProcess)}, nil
}

func (tracker *processTracker) Discover(root int) error {
	if tracker.root == 0 {
		tracker.root = root
	}
	all, err := readProcessTable()
	if err != nil {
		return fmt.Errorf("inspect native process tree: %w", err)
	}
	related := descendants(all, root)
	for pid, item := range descendants(all, tracker.owner) {
		if pid != tracker.owner && !underOtherNativeRoot(all, pid, tracker.root) {
			related[pid] = item
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

func (tracker *processTracker) Live() (int, error) {
	live := 0
	for pid, item := range tracker.tracked {
		current, err := readProcess(pid)
		if errors.Is(err, os.ErrNotExist) {
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
		if errors.Is(err, os.ErrNotExist) {
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
		current, err := readProcess(pid)
		if errors.Is(err, os.ErrNotExist) {
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
		if current.state == 'Z' {
			continue
		}
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
		current, err := readProcess(pid)
		if errors.Is(err, os.ErrNotExist) {
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
	if tracker.root != 0 {
		unregisterNativeRoot(tracker.root)
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
		item, statErr := readProcess(pid)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return nil, statErr
		}
		result[pid] = item
	}
	return result, nil
}

func readProcess(pid int) (trackedProcess, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
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
	return trackedProcess{pid: pid, start: fields[19], parent: parent, state: fields[0][0]}, nil
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
