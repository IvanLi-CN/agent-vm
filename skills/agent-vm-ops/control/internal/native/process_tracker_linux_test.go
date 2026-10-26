//go:build linux

package native

import "testing"

func TestReadProcessTableAllowsKernelThreads(t *testing.T) {
	processes, err := readProcessTable()
	if err != nil {
		t.Fatalf("readProcessTable() error = %v", err)
	}
	if len(processes) == 0 {
		t.Fatal("readProcessTable() returned no processes")
	}
}
