//go:build linux

package native

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

func workerIdentity(pid int) (WorkerIdentity, error) {
	process, err := readProcess(pid)
	if err != nil {
		return WorkerIdentity{}, err
	}
	return WorkerIdentity{PID: process.pid, StartTime: process.start, ProcessGroup: process.group}, nil
}

func WorkerAlive(identity WorkerIdentity) (bool, error) {
	if identity.PID <= 0 || identity.StartTime == "" || identity.ProcessGroup <= 0 {
		return false, errors.New("invalid native worker identity")
	}
	process, err := readProcess(identity.PID)
	if processGone(err) {
		return workerGroupAlive(identity.ProcessGroup, readProcessTable)
	}
	if err != nil {
		return false, err
	}
	if process.start != identity.StartTime {
		return false, fmt.Errorf("native worker identity changed for pid %d", identity.PID)
	}
	if process.group != identity.ProcessGroup {
		return false, fmt.Errorf("native worker process group changed for pid %d", identity.PID)
	}
	if process.state != 'Z' {
		return true, nil
	}
	return workerGroupAlive(identity.ProcessGroup, readProcessTable)
}

func workerGroupAlive(group int, readTable func() (map[int]trackedProcess, error)) (bool, error) {
	all, err := readTable()
	if err != nil {
		return false, err
	}
	sawZombie := false
	for _, candidate := range all {
		if candidate.group != group {
			continue
		}
		if candidate.state != 'Z' {
			return true, nil
		}
		sawZombie = true
	}
	if sawZombie {
		// Refresh once before treating a zombie-only snapshot as gone. The
		// kernel probe below must still run after the refresh because both
		// procfs snapshots can miss an active member.
		refreshed, refreshErr := readTable()
		if refreshErr != nil {
			return false, refreshErr
		}
		for _, candidate := range refreshed {
			if candidate.group != group {
				continue
			}
			if candidate.state != 'Z' {
				return true, nil
			}
		}
	}
	// A procfs table is a point-in-time enumeration: a surviving member can
	// fork after enumeration and exit before its stat file is read. The kernel
	// process-group probe closes that false-empty window when procfs has no
	// live group members after the optional zombie refresh.
	if err := unix.Kill(-group, 0); err == nil || errors.Is(err, unix.EPERM) {
		return true, nil
	} else if !errors.Is(err, unix.ESRCH) {
		return false, err
	}
	return false, nil
}

type workerKillOps struct {
	openPIDFD    func(int) (int, error)
	closePIDFD   func(int) error
	readIdentity func(int) (WorkerIdentity, error)
	workerAlive  func(WorkerIdentity) (bool, error)
	sendSignal   func(int) error
	killGroup    func(WorkerIdentity, *workerGroupCleanup) error
	groupAlive   func(int) (bool, error)
	sleep        func(time.Duration)
}

type workerGroupCleanup struct {
	rootPIDFD    int
	rootSignaled bool
	members      map[int]trackedProcess
}

type processGroupKillOps struct {
	readTable   func() (map[int]trackedProcess, error)
	openPIDFD   func(int) (int, error)
	closePIDFD  func(int) error
	readProcess func(int) (trackedProcess, error)
	sendSignal  func(int) error
}

func workerRootExited(identity WorkerIdentity, workerAlive func(WorkerIdentity) (bool, error)) error {
	alive, err := workerAlive(identity)
	if err != nil {
		return err
	}
	if alive {
		return fmt.Errorf("native worker root exited while its process group remains; manual reconciliation required")
	}
	return nil
}

func KillWorker(identity WorkerIdentity) error {
	return killWorkerWith(identity, workerKillOps{
		openPIDFD:    func(pid int) (int, error) { return unix.PidfdOpen(pid, 0) },
		closePIDFD:   unix.Close,
		readIdentity: workerIdentity,
		workerAlive:  WorkerAlive,
		sendSignal: func(fd int) error {
			return unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
		},
		killGroup: killProcessGroup,
		groupAlive: func(group int) (bool, error) {
			return workerGroupAlive(group, readProcessTable)
		},
		sleep: time.Sleep,
	})
}

func killWorkerWith(identity WorkerIdentity, ops workerKillOps) error {
	if identity.PID <= 0 || identity.StartTime == "" || identity.ProcessGroup <= 0 {
		return errors.New("invalid native worker identity")
	}
	fd, err := ops.openPIDFD(identity.PID)
	if errors.Is(err, unix.ESRCH) {
		return workerRootExited(identity, ops.workerAlive)
	}
	if err != nil {
		return fmt.Errorf("open native worker pidfd: %w", err)
	}
	defer ops.closePIDFD(fd)
	current, err := ops.readIdentity(identity.PID)
	if processGone(err) {
		return workerRootExited(identity, ops.workerAlive)
	}
	if err != nil {
		return fmt.Errorf("recheck native worker identity: %w", err)
	}
	if current.StartTime != identity.StartTime {
		return fmt.Errorf("native worker identity changed for pid %d", identity.PID)
	}
	if current.ProcessGroup != identity.ProcessGroup {
		return fmt.Errorf("native worker process group changed for pid %d", identity.PID)
	}
	if current.ProcessGroup == current.PID {
		cleanup := &workerGroupCleanup{rootPIDFD: fd, members: make(map[int]trackedProcess)}
		if err := ops.killGroup(current, cleanup); err != nil && !errors.Is(err, unix.ESRCH) {
			return fmt.Errorf("kill native worker process group: %w", err)
		}
		if !cleanup.rootSignaled {
			if err := ops.sendSignal(fd); err != nil && !errors.Is(err, unix.ESRCH) {
				return fmt.Errorf("kill native worker: %w", err)
			}
			cleanup.rootSignaled = true
		}
		if ops.groupAlive != nil {
			for attempt := 0; attempt < 10; attempt++ {
				alive, aliveErr := ops.groupAlive(current.ProcessGroup)
				if aliveErr != nil {
					return fmt.Errorf("inspect native worker process group after cleanup: %w", aliveErr)
				}
				if !alive {
					return nil
				}
				if attempt == 9 {
					break
				}
				if err := ops.killGroup(current, cleanup); err != nil && !errors.Is(err, unix.ESRCH) {
					return fmt.Errorf("retry native worker process-group cleanup: %w", err)
				}
				if ops.sleep != nil {
					ops.sleep(10 * time.Millisecond)
				}
			}
			return errors.New("native worker process group remains after cleanup")
		}
		return nil
	}
	if err := ops.sendSignal(fd); err != nil {
		if errors.Is(err, unix.ESRCH) {
			return workerRootExited(identity, ops.workerAlive)
		}
		return fmt.Errorf("kill native worker: %w", err)
	}
	return nil
}

// killProcessGroup uses pidfds for each member while the original root pidfd
// anchors the group. The root is signalled last so it cannot disappear before
// the first member scan. Once it is gone, retries only admit descendants of
// already verified members; a reused numeric PGID is never enough to claim a
// new process.
func killProcessGroup(identity WorkerIdentity, cleanup *workerGroupCleanup) error {
	return killProcessGroupWith(identity, cleanup, processGroupKillOps{
		readTable:   readProcessTable,
		openPIDFD:   func(pid int) (int, error) { return unix.PidfdOpen(pid, 0) },
		closePIDFD:  unix.Close,
		readProcess: readProcess,
		sendSignal: func(fd int) error {
			return unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
		},
	})
}

func killProcessGroupWith(identity WorkerIdentity, cleanup *workerGroupCleanup, ops processGroupKillOps) error {
	if cleanup == nil {
		return errors.New("native worker group cleanup state is missing")
	}
	if ops.readTable == nil || ops.openPIDFD == nil || ops.closePIDFD == nil || ops.readProcess == nil || ops.sendSignal == nil {
		return errors.New("native worker group cleanup operations are incomplete")
	}
	if cleanup.members == nil {
		cleanup.members = make(map[int]trackedProcess)
	}
	all, err := ops.readTable()
	if err != nil {
		return err
	}
	root, rootPresent := all[identity.PID]
	if rootPresent {
		if root.start != identity.StartTime {
			return fmt.Errorf("native worker identity changed for pid %d", identity.PID)
		}
		if root.group != identity.ProcessGroup {
			return fmt.Errorf("native worker process group changed for pid %d", identity.PID)
		}
		currentRoot, rootErr := ops.readProcess(identity.PID)
		if processGone(rootErr) {
			// The table retained a stale root entry. Do not claim any members
			// from a snapshot that may already contain a reused numeric PGID.
			return nil
		}
		if rootErr != nil {
			return fmt.Errorf("recheck native worker root: %w", rootErr)
		}
		if currentRoot.start != identity.StartTime {
			return fmt.Errorf("native worker identity changed for pid %d", identity.PID)
		}
		if currentRoot.group != identity.ProcessGroup {
			return fmt.Errorf("native worker process group changed for pid %d", identity.PID)
		}
	} else if !cleanup.rootSignaled {
		// Do not claim members from a root-less first scan. A new process group
		// can reuse the numeric PGID after the original leader exits, even while
		// the original root pidfd remains open. The caller will signal that
		// pidfd and fail closed if any unanchored group member remains.
		return nil
	}
	if rootPresent {
		for pid, item := range all {
			if pid != identity.PID && item.group == identity.ProcessGroup && item.state != 'Z' {
				cleanup.members[pid] = item
			}
		}
	} else {
		// A root-less retry may only add a child whose current parent is an
		// already verified member. This keeps a reused numeric PGID from
		// becoming an ownership claim after the root has exited.
		for pid, item := range all {
			if item.group != identity.ProcessGroup || item.state == 'Z' {
				continue
			}
			if _, known := cleanup.members[pid]; known {
				continue
			}
			parent, parentPresent := all[item.parent]
			knownParent, known := cleanup.members[item.parent]
			if parentPresent && known && parent.start == knownParent.start && parent.group == identity.ProcessGroup {
				cleanup.members[pid] = item
			}
		}
	}
	var first error
	for pid, item := range cleanup.members {
		current, currentPresent := all[pid]
		if !currentPresent {
			continue
		}
		if current.start != item.start {
			if first == nil {
				first = fmt.Errorf("process-group member identity changed for pid %d", pid)
			}
			continue
		}
		if current.group != identity.ProcessGroup {
			if first == nil {
				first = fmt.Errorf("process-group member group changed for pid %d", pid)
			}
			continue
		}
		fd, openErr := ops.openPIDFD(pid)
		if errors.Is(openErr, unix.ESRCH) {
			continue
		}
		if openErr != nil {
			if first == nil {
				first = fmt.Errorf("open pidfd for process-group member %d: %w", pid, openErr)
			}
			continue
		}
		current, readErr := ops.readProcess(pid)
		if processGone(readErr) {
			_ = ops.closePIDFD(fd)
			continue
		}
		if readErr != nil {
			_ = ops.closePIDFD(fd)
			if first == nil {
				first = fmt.Errorf("recheck process-group member %d: %w", pid, readErr)
			}
			continue
		}
		if current.start != item.start || current.group != identity.ProcessGroup {
			_ = ops.closePIDFD(fd)
			if first == nil {
				first = fmt.Errorf("process-group member identity changed for pid %d", pid)
			}
			continue
		}
		signalErr := ops.sendSignal(fd)
		_ = ops.closePIDFD(fd)
		if signalErr != nil && !errors.Is(signalErr, unix.ESRCH) && first == nil {
			first = fmt.Errorf("kill process-group member %d: %w", pid, signalErr)
		}
	}
	if rootPresent && !cleanup.rootSignaled {
		if cleanup.rootPIDFD < 0 {
			return errors.New("native worker root pidfd is missing")
		}
		signalErr := ops.sendSignal(cleanup.rootPIDFD)
		if signalErr != nil && !errors.Is(signalErr, unix.ESRCH) {
			if first == nil {
				first = fmt.Errorf("kill native worker root: %w", signalErr)
			}
		} else {
			cleanup.rootSignaled = true
		}
	}
	return first
}
