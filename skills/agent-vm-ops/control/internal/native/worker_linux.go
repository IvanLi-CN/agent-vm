//go:build linux

package native

import (
	"errors"
	"fmt"

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
	for _, candidate := range all {
		if candidate.group == group && candidate.state != 'Z' {
			return true, nil
		}
	}
	// A procfs table is a point-in-time enumeration: a surviving member can
	// fork after enumeration and exit before its stat file is read. The kernel
	// process-group probe closes that false-empty window. If the group exists
	// but only exposes zombies, stay conservative and require reconciliation.
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
	killGroup    func(WorkerIdentity) error
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
	if err := ops.sendSignal(fd); err != nil {
		if errors.Is(err, unix.ESRCH) {
			return workerRootExited(identity, ops.workerAlive)
		}
		return fmt.Errorf("kill native worker: %w", err)
	}
	if current.ProcessGroup == current.PID {
		if err := ops.killGroup(current); err != nil && !errors.Is(err, unix.ESRCH) {
			return fmt.Errorf("kill native worker process group: %w", err)
		}
	}
	return nil
}

// killProcessGroup uses pidfds for each member after anchoring the group to
// the original root identity. A numeric PGID is not sufficient after the root
// exits because the same PID can later become an unrelated process group.
func killProcessGroup(identity WorkerIdentity) error {
	all, err := readProcessTable()
	if err != nil {
		return err
	}
	root, ok := all[identity.PID]
	if !ok {
		return workerRootExited(identity, WorkerAlive)
	}
	if root.start != identity.StartTime {
		return fmt.Errorf("native worker identity changed for pid %d", identity.PID)
	}
	if root.group != identity.ProcessGroup {
		return fmt.Errorf("native worker process group changed for pid %d", identity.PID)
	}
	var first error
	for pid, item := range all {
		if item.group != identity.ProcessGroup || item.state == 'Z' {
			continue
		}
		fd, openErr := unix.PidfdOpen(pid, 0)
		if errors.Is(openErr, unix.ESRCH) {
			continue
		}
		if openErr != nil {
			if first == nil {
				first = fmt.Errorf("open pidfd for process-group member %d: %w", pid, openErr)
			}
			continue
		}
		current, readErr := readProcess(pid)
		if processGone(readErr) {
			_ = unix.Close(fd)
			continue
		}
		if readErr != nil {
			_ = unix.Close(fd)
			if first == nil {
				first = fmt.Errorf("recheck process-group member %d: %w", pid, readErr)
			}
			continue
		}
		if current.start != item.start || current.group != identity.ProcessGroup {
			_ = unix.Close(fd)
			if first == nil {
				first = fmt.Errorf("process-group member identity changed for pid %d", pid)
			}
			continue
		}
		signalErr := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
		_ = unix.Close(fd)
		if signalErr != nil && !errors.Is(signalErr, unix.ESRCH) && first == nil {
			first = fmt.Errorf("kill process-group member %d: %w", pid, signalErr)
		}
	}
	return first
}
