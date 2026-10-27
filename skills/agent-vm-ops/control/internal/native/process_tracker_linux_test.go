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

func TestReadProcessTableSkipsProcfsExit(t *testing.T) {
	injected := false
	processes, err := readProcessTableWith(func(path string) ([]byte, error) {
		if !injected {
			injected = true
			return nil, &os.PathError{Op: "read", Path: path, Err: unix.ESRCH}
		}
		return os.ReadFile(path)
	})
	if err != nil {
		t.Fatalf("readProcessTableWith() error = %v", err)
	}
	if !injected {
		t.Fatal("readProcessTableWith() did not exercise the injected procfs read")
	}
	if len(processes) == 0 {
		t.Fatal("readProcessTableWith() returned no processes after skipping the exited process")
	}
}

func TestReadProcessTablePropagatesProcfsReadErrors(t *testing.T) {
	sentinel := errors.New("synthetic procfs read failure")
	_, err := readProcessTableWith(func(string) ([]byte, error) {
		return nil, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("readProcessTableWith() error = %v, want %v", err, sentinel)
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
