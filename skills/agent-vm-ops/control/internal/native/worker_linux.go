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
	return false, nil
}

type workerKillOps struct {
	openPIDFD    func(int) (int, error)
	closePIDFD   func(int) error
	readIdentity func(int) (WorkerIdentity, error)
	workerAlive  func(WorkerIdentity) (bool, error)
	sendSignal   func(int) error
	killGroup    func(int) error
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
		killGroup: func(group int) error {
			return unix.Kill(-group, unix.SIGKILL)
		},
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
	if err := ops.sendSignal(fd); err != nil && !errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("kill native worker: %w", err)
	}
	if current.ProcessGroup == current.PID {
		if err := ops.killGroup(current.ProcessGroup); err != nil && !errors.Is(err, unix.ESRCH) {
			return fmt.Errorf("kill native worker process group: %w", err)
		}
	}
	return nil
}
