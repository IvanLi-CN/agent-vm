//go:build darwin || freebsd || openbsd || netbsd

package native

import "syscall"

func killGroup(pid int) error {
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if err != nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	return err
}
