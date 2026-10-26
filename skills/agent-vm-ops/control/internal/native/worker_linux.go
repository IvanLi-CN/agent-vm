//go:build linux

package native

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func workerIdentity(pid int) (WorkerIdentity, error) {
	process, err := readProcess(pid)
	if err != nil {
		return WorkerIdentity{}, err
	}
	return WorkerIdentity{PID: process.pid, StartTime: process.start}, nil
}

func WorkerAlive(identity WorkerIdentity) (bool, error) {
	if identity.PID <= 0 || identity.StartTime == "" {
		return false, errors.New("invalid native worker identity")
	}
	process, err := readProcess(identity.PID)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if process.start != identity.StartTime {
		return false, fmt.Errorf("native worker identity changed for pid %d", identity.PID)
	}
	return process.state != 'Z', nil
}

func KillWorker(identity WorkerIdentity) error {
	alive, err := WorkerAlive(identity)
	if err != nil || !alive {
		return err
	}
	fd, err := unix.PidfdOpen(identity.PID, 0)
	if err != nil && !errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("open native worker pidfd: %w", err)
	}
	if fd >= 0 {
		defer unix.Close(fd)
	}
	if err := unix.Kill(-identity.PID, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("kill native worker group: %w", err)
	}
	if fd >= 0 {
		if err := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
			return fmt.Errorf("kill native worker: %w", err)
		}
	}
	return nil
}
