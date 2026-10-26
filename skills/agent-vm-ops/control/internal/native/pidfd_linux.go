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
	if pidfd >= 0 {
		_ = unix.PidfdSendSignal(pidfd, unix.SIGKILL, nil, 0)
		return
	}
	// Kernels without pidfd support retain the process-group fallback. On
	// supported kernels, cleanupTracked handles descendants with identity checks.
	_ = unix.Kill(-pid, unix.SIGKILL)
	_ = unix.Kill(pid, unix.SIGKILL)
}
