//go:build linux

package native

import (
	"golang.org/x/sys/unix"
)

func init() { _ = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0) }

func openPIDFD(pid int) int {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return -1
	}
	return fd
}

func closePIDFD(fd int) {
	if fd >= 0 {
		_ = unix.Close(fd)
	}
}

func killProcess(pid, pidfd int) {
	// Kill the controlled process group first. A pidfd identifies only the
	// leader; using it first leaves shell children alive with inherited pipes.
	_ = unix.Kill(-pid, unix.SIGKILL)
	if pidfd >= 0 {
		_ = unix.PidfdSendSignal(pidfd, unix.SIGKILL, nil, 0)
	}
	_ = unix.Kill(pid, unix.SIGKILL)
}
