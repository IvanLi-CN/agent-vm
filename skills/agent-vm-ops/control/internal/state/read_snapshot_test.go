package state

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestReadSnapshotSurvivesConcurrentCommitAndRejectsEscapedContext(t *testing.T) {
	s, err := OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var escaped context.Context
	err = s.ReadSnapshot(ctx, func(view context.Context) error {
		escaped = view
		before, err := s.StateEpoch(view)
		if err != nil {
			return err
		}
		if err := s.SetMeta(view, "forbidden", "value"); err == nil {
			t.Fatal("read snapshot allowed mutation")
		}
		written := make(chan error, 1)
		go func() { written <- s.SetMeta(ctx, "concurrent", "value") }()
		select {
		case err := <-written:
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
		after, err := s.StateEpoch(view)
		if err != nil {
			return err
		}
		if before != after {
			t.Fatalf("snapshot changed across a concurrent commit: %s -> %s", before, after)
		}
		if _, found, err := s.Meta(view, "concurrent"); err != nil {
			return err
		} else if found {
			t.Fatal("new row entered an existing snapshot")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.StateEpoch(escaped); err == nil {
		t.Fatal("escaped snapshot remained usable")
	}
	if _, found, err := s.Meta(ctx, "concurrent"); err != nil || !found {
		t.Fatalf("concurrent commit missing after snapshot: found=%v err=%v", found, err)
	}
}

func TestReadSnapshotCancellationReleasesConnection(t *testing.T) {
	s, err := OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	if err := s.ReadSnapshot(ctx, func(view context.Context) error {
		if _, err := s.StateEpoch(view); err != nil {
			return err
		}
		cancel()
		return view.Err()
	}); err == nil {
		t.Fatal("cancellation disappeared")
	}
	if err := s.SetMeta(context.Background(), "after", "value"); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteReadSnapshotReleasesAuthorityLockBeforeProbes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"results": []any{map[string]any{"columns": []string{"value"}, "types": []string{"text"}, "values": [][]string{{"initial"}}}}})
	}))
	defer server.Close()
	store, err := newRemoteStoreWithClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.remote.mirror.Exec("CREATE TABLE meta(key TEXT PRIMARY KEY,value TEXT); INSERT INTO meta VALUES('state_epoch','initial'),('management_auth_epoch','initial')"); err != nil {
		t.Fatal(err)
	}
	store.remote.syncedEpoch = "initial"
	store.remote.syncedAuthEpoch = "initial"
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := store.ReadSnapshot(ctx, func(view context.Context) error {
		if err := store.remote.mu.LockContext(ctx); err != nil {
			return err
		}
		_, err := store.remote.mirror.ExecContext(ctx, "UPDATE meta SET value='later' WHERE key='state_epoch'")
		store.remote.mu.Unlock()
		if err != nil {
			return err
		}
		epoch, err := store.StateEpoch(view)
		if err == nil && epoch != "initial" {
			t.Fatalf("detached snapshot changed to %s", epoch)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
