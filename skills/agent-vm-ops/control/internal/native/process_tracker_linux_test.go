//go:build linux

package native

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestProcessGoneRecognizesProcfsExitErrors(t *testing.T) {
	for name, err := range map[string]error{
		"missing path": os.ErrNotExist,
		"procfs exit":  unix.ESRCH,
	} {
		t.Run(name, func(t *testing.T) {
			if !processGone(err) {
				t.Fatalf("processGone(%v) = false", err)
			}
		})
	}
	if processGone(errors.New("unrelated process error")) {
		t.Fatal("processGone() accepted an unrelated error")
	}
}

func TestReadProcessTableAllowsKernelThreads(t *testing.T) {
	processes, err := readProcessTable()
	if err != nil {
		t.Fatalf("readProcessTable() error = %v", err)
	}
	if len(processes) == 0 {
		t.Fatal("readProcessTable() returned no processes")
	}
}
