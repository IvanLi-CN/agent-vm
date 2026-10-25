//go:build !darwin && !linux

package native

import "os/exec"

func configureCommand(_ *exec.Cmd) {}
