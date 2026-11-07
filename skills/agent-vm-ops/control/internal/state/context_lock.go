package state

import (
	"context"
	"sync"
)

// contextMutex bounds queueing as well as the I/O performed under the lock.
// It never starts a goroutine that could acquire an abandoned lock later.
type contextMutex struct {
	once  sync.Once
	token chan struct{}
}

func (m *contextMutex) LockContext(ctx context.Context) error {
	m.once.Do(func() { m.token = make(chan struct{}, 1) })
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case m.token <- struct{}{}:
		return nil
	}
}
func (m *contextMutex) Lock()   { _ = m.LockContext(context.Background()) }
func (m *contextMutex) Unlock() { <-m.token }
