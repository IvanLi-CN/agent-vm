package native

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
)

func TestNamedLockIsProcessIndependentAndContextAware(t *testing.T) {
	directory := t.TempDir()
	first, err := AcquireNamedLock(context.Background(), directory, "session:one")
	if err != nil {
		t.Fatal(err)
	}
	if _, acquired, err := TryNamedLock(directory, "session:one"); err != nil || acquired {
		t.Fatalf("busy named lock acquired=%v err=%v", acquired, err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := AcquireNamedLock(canceled, directory, "session:one"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled named lock error=%v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, acquired, err := TryNamedLock(directory, "session:one")
	if err != nil || !acquired || second == nil {
		t.Fatalf("released named lock acquired=%v lock=%v err=%v", acquired, second != nil, err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNamedLockAcrossProcess(t *testing.T) {
	if os.Getenv("AGENT_VM_NAMED_LOCK_HELPER") == "1" {
		_, acquired, err := TryNamedLock(os.Getenv("AGENT_VM_NAMED_LOCK_DIR"), "cross-process")
		if err != nil {
			os.Exit(2)
		}
		if acquired {
			os.Exit(1)
		}
		os.Exit(0)
	}

	directory := t.TempDir()
	lock, err := AcquireNamedLock(context.Background(), directory, "cross-process")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	command := exec.Command(os.Args[0], "-test.run=^TestNamedLockAcrossProcess$")
	command.Env = append(os.Environ(), "AGENT_VM_NAMED_LOCK_HELPER=1", "AGENT_VM_NAMED_LOCK_DIR="+directory)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("helper process acquired a busy lock: err=%v output=%s", err, output)
	}
}
