//go:build linux

package native

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestReadProcessTableAllowsKernelThreads(t *testing.T) {
	processes, err := readProcessTable()
	if err != nil {
		t.Fatalf("readProcessTable() error = %v", err)
	}
	if len(processes) == 0 {
		t.Fatal("readProcessTable() returned no processes")
	}
}

func TestProcessMissingRecognizesProcExitRaces(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "enoent", err: os.ErrNotExist, want: true},
		{name: "esrch", err: unix.ESRCH, want: true},
		{name: "wrapped esrch", err: fmt.Errorf("proc scan: %w", unix.ESRCH), want: true},
		{name: "other", err: errors.New("permission denied"), want: false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := isProcessMissing(test.err); got != test.want {
				t.Fatalf("isProcessMissing(%v) = %v, want %v", test.err, got, test.want)
			}
		})
	}
}
