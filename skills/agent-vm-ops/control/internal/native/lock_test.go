package native

import (
	"context"
	"errors"
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
