package state

import (
	"context"
	"encoding/json"
	"errors"
)

// OpenFreshRQLite initializes a new authority without an on-disk SQLite source.
// Existing schemas require the normal open or explicit migration/recovery path.
func OpenFreshRQLite(ctx context.Context, dsn string) (*Store, error) {
	store, err := newRemoteStore(dsn)
	if err != nil {
		return nil, err
	}
	objects, err := store.remote.remoteSchema(ctx)
	if err != nil {
		store.Close()
		return nil, err
	}
	if len(objects) != 0 {
		store.Close()
		return nil, errors.New("rqlite is not empty; fresh initialization refused")
	}
	if err := store.remote.open(ctx, true); err != nil {
		store.Close()
		return nil, err
	}
	return store, nil
}

// ResumeFreshRQLite is used only by explicit initial-installation recovery.
// Empty authorities can be initialized; supported existing authorities must
// have no VM tasks or lifecycle operations. No database is replaced or reset.
func ResumeFreshRQLite(ctx context.Context, dsn string) (*Store, error) {
	s, err := newRemoteStore(dsn)
	if err != nil {
		return nil, err
	}
	objects, err := s.remote.remoteSchema(ctx)
	if err != nil {
		s.Close()
		return nil, err
	}
	if len(objects) == 0 {
		s.Close()
		return OpenFreshRQLite(ctx, dsn)
	}
	if err := requireSupportedSchema(ctx, objects); err != nil {
		s.Close()
		return nil, err
	}
	result, err := s.remote.query(ctx, "SELECT (SELECT COUNT(*) FROM tasks) + (SELECT COUNT(*) FROM operations)")
	if err != nil {
		s.Close()
		return nil, err
	}
	// Decode using the same SQL result representation as other state probes.
	if len(result.Values) != 1 || len(result.Values[0]) != 1 {
		s.Close()
		return nil, errors.New("initial authority count is unavailable")
	}
	value, ok := result.Values[0][0].(json.Number)
	count, countErr := value.Int64()
	if !ok || countErr != nil || count != 0 {
		s.Close()
		return nil, errors.New("initial authority already contains lifecycle state")
	}
	if err := s.remote.open(ctx, false); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}
