package native

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLimitedBufferReportsTheWholeInputWhenTruncated(t *testing.T) {
	buffer := &limitedBuffer{limit: 3}
	input := []byte("abcdef")
	written, err := buffer.Write(input)
	if err != nil {
		t.Fatal(err)
	}
	if written != len(input) {
		t.Fatalf("Write returned %d, want %d", written, len(input))
	}
	if string(buffer.Bytes()) != "abc" || !buffer.Truncated {
		t.Fatalf("buffer = %q, truncated = %v", buffer.Bytes(), buffer.Truncated)
	}
}

func TestTimeoutKillsTheWholeProcessGroupAndBoundsPipeCleanup(t *testing.T) {
	runner := NewRunner()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := runner.Run(ctx, []string{"/bin/sh", "-c", "sleep 30 & wait"}, nil)
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("Run error = %v, want a timeout", err)
	}
	if elapsed := time.Since(started); elapsed > 4*time.Second {
		t.Fatalf("timeout cleanup took %s", elapsed)
	}
}

func TestEscapedDescendantIsDetectedAndCleaned(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux process ownership boundary required")
	}
	pidPath := filepath.Join(t.TempDir(), "pid")
	runner := NewRunner()
	command := "/bin/sh -c 'echo $$ > \"$1\"; sleep 30' sh '" + pidPath + "'"
	_, err := runner.Run(context.Background(), []string{"/bin/sh", "-c", "setsid " + command + " & exit 0"}, nil)
	if err == nil || !strings.Contains(err.Error(), "child processes") {
		t.Fatalf("Run error = %v, want escaped child failure", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	var lastPID int
	var lastStat []byte
	for time.Now().Before(deadline) {
		raw, readErr := os.ReadFile(pidPath)
		if readErr == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(raw)))
			if parseErr == nil {
				lastPID = pid
				lastStat, _ = os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
				if _, statErr := os.Stat("/proc/" + strconv.Itoa(pid)); os.IsNotExist(statErr) {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("escaped descendant still exists after runner cleanup: pid=%d stat=%q", lastPID, lastStat)
}
