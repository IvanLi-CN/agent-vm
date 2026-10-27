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
	if identity.PID <= 0 || identity.StartTime == "" {
		return false, errors.New("invalid native worker identity")
	}
	process, err := readProcess(identity.PID)
	if isProcessMissing(err) {
		if identity.ProcessGroup <= 0 {
			return false, nil
		}
		all, tableErr := readProcessTable()
		if tableErr != nil {
			return false, tableErr
		}
		for _, candidate := range all {
			if candidate.group == identity.ProcessGroup && candidate.state != 'Z' {
				return true, nil
			}
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if process.start != identity.StartTime {
		return false, fmt.Errorf("native worker identity changed for pid %d", identity.PID)
	}
	if identity.ProcessGroup > 0 && process.group != identity.ProcessGroup {
		return false, fmt.Errorf("native worker process group changed for pid %d", identity.PID)
	}
	if process.state != 'Z' {
		return true, nil
	}
	if identity.ProcessGroup <= 0 {
		return false, nil
	}
	all, err := readProcessTable()
	if err != nil {
		return false, err
	}
	for _, candidate := range all {
		if candidate.group == identity.ProcessGroup && candidate.state != 'Z' {
			return true, nil
		}
	}
	return false, nil
}

func KillWorker(identity WorkerIdentity) error {
	fd, err := unix.PidfdOpen(identity.PID, 0)
	if errors.Is(err, unix.ESRCH) {
		alive, aliveErr := WorkerAlive(identity)
		if aliveErr != nil {
			return aliveErr
		}
		if alive {
			return fmt.Errorf("native worker root exited while its process group remains; manual reconciliation required")
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("open native worker pidfd: %w", err)
	}
	defer unix.Close(fd)
	current, err := workerIdentity(identity.PID)
	if isProcessMissing(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("recheck native worker identity: %w", err)
	}
	if current.StartTime != identity.StartTime {
		return fmt.Errorf("native worker identity changed for pid %d", identity.PID)
	}
	if identity.ProcessGroup > 0 && current.ProcessGroup != identity.ProcessGroup {
		return fmt.Errorf("native worker process group changed for pid %d", identity.PID)
	}
	if err := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("kill native worker: %w", err)
	}
	if current.ProcessGroup > 0 && current.ProcessGroup == current.PID {
		if err := unix.Kill(-current.ProcessGroup, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
			return fmt.Errorf("kill native worker process group: %w", err)
		}
	}
	return nil
}
