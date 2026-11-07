package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
// authority is synchronized once, then copied into an independent transient
// read-only view so native probes cannot block authority writes. The context must not escape the callback.
func (s *Store) ReadSnapshot(ctx context.Context, fn func(context.Context) error) error {
	if view, ok := ctx.Value(readSnapshotKey{}).(*readSnapshot); ok && view.store == s {
		if !view.active.Load() {
			return errors.New("read snapshot is closed")
		}
		return fn(ctx)
	}

	if s.remote != nil {
		// Detach the synchronized in-memory mirror before external native probes.
		// No callback may hold the authority mutex for the duration of those probes.
		var data []byte
		if err := s.withRead(ctx, func(conn *sql.Conn) error {
			return conn.Raw(func(driver any) error {
				serializer, ok := driver.(interface{ Serialize() ([]byte, error) })
				if !ok {
					return errors.New("SQLite snapshot serialization unavailable")
				}
				var err error
				data, err = serializer.Serialize()
				return err
			})
		}); err != nil {
			return err
		}
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			return err
		}
		defer db.Close()
		conn, err := db.Conn(ctx)
		if err != nil {
			return err
		}
		defer conn.Close()
		if err := conn.Raw(func(driver any) error {
			deserializer, ok := driver.(interface{ Deserialize([]byte) error })
			if !ok {
				return errors.New("SQLite snapshot deserialization unavailable")
			}
			return deserializer.Deserialize(data)
		}); err != nil {
			return fmt.Errorf("detach read snapshot: %w", err)
		}
		return s.readSnapshotOnConn(ctx, conn, fn)
	}
	return s.withRead(ctx, func(conn *sql.Conn) error { return s.readSnapshotOnConn(ctx, conn, fn) })
}

func (s *Store) readSnapshotOnConn(ctx context.Context, conn *sql.Conn, fn func(context.Context) error) (err error) {

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
}
