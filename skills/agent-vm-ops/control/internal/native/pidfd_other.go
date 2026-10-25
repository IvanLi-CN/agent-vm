//go:build !linux

package native

func openPIDFD(_ int) int    { return -1 }
func closePIDFD(_ int)       {}
func killProcess(pid, _ int) { _ = killGroup(pid) }
