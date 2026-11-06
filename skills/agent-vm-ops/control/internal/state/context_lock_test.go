package state

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestContextMutexCanceledWaitNeverAcquiresLater(t *testing.T) {
	var lock contextMutex
	lock.Lock()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if err := lock.LockContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait error=%v", err)
	}
	lock.Unlock()
	fresh, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	if err := lock.LockContext(fresh); err != nil {
		t.Fatal(err)
	}
	lock.Unlock()
}

func TestManagementSchemaLockWaitHonorsDeadline(t *testing.T) {
	backend := &remoteBackend{}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if err := backend.ensureManagementSchema(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("schema lock error=%v", err)
	}
}
