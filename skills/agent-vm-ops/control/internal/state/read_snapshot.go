package state

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"time"
)

type readSnapshotKey struct{}
type readSnapshot struct {
	store  *Store
	conn   *sql.Conn
	active atomic.Bool
}

// ReadSnapshot gives related observations one read-only state view. The remote
// authority is synchronized once; writes cannot change its mirror until the
// callback returns. The context must not escape the callback.
func (s *Store) ReadSnapshot(ctx context.Context, fn func(context.Context) error) error {
	if view, ok := ctx.Value(readSnapshotKey{}).(*readSnapshot); ok && view.store == s {
		if !view.active.Load() {
			return errors.New("read snapshot is closed")
		}
		return fn(ctx)
	}
	return s.withRead(ctx, func(conn *sql.Conn) (err error) {
		if _, err = conn.ExecContext(ctx, "BEGIN"); err != nil {
			return err
		}
		view := &readSnapshot{store: s, conn: conn}
		view.active.Store(true)
		defer func() {
			view.active.Store(false)
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_, rollbackErr := conn.ExecContext(cleanup, "ROLLBACK")
			err = errors.Join(err, rollbackErr)
		}()
		return fn(context.WithValue(ctx, readSnapshotKey{}, view))
	})
}
