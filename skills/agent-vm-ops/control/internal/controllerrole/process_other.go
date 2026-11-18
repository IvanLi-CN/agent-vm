//go:build !linux

package controllerrole

import (
	"os"
	"syscall"
)

func processStartToken(int) string { return "" }

func liveProcessMatches(pid int, startTime string) bool {
	process, err := os.FindProcess(pid)
	return err == nil && process.Signal(syscall.Signal(0)) == nil && startTime == ""
}
