package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-toolkit/skills/agent-vm-ops/control/internal/config"
	"github.com/IvanLi-CN/agent-toolkit/skills/agent-vm-ops/control/internal/lifecycle"
	"github.com/IvanLi-CN/agent-toolkit/skills/agent-vm-ops/control/internal/observe"
	"github.com/IvanLi-CN/agent-toolkit/skills/agent-vm-ops/control/internal/state"
)

func TestUDSControlHealthAndSnapshotRoutes(t *testing.T) {
	root := t.TempDir()
	store, err := state.OpenForTest(filepath.Join(root, "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.StateDB = store.Path()
	snapshots := observe.NewSnapshotStore(filepath.Join(root, "metrics"))
	service := lifecycle.New(store, cfg, lifecycle.Options{Snapshots: snapshots})
	service.MarkReady()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// macOS limits AF_UNIX paths to a small fixed size; keep the test socket
	// short while still exercising the service's parent-directory creation.
	socket := filepath.Join(root, "s")
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- Serve(ctx, socket, service, snapshots) }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, statErr := os.Stat(socket); statErr == nil {
			break
		}
		select {
		case serveErr := <-serverErrors:
			t.Fatalf("serve exited before creating socket: %v", serveErr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("control socket was not created")
		}
		time.Sleep(5 * time.Millisecond)
	}

	body, status, err := Request(context.Background(), socket, []byte(`{"protocol":4,"action":"status","session_id":"http-test"}`))
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("control status = %d, body = %s", status, body)
	}
	var control map[string]any
	if err := json.Unmarshal(body, &control); err != nil {
		t.Fatal(err)
	}
	if control["ok"] != true || control["action"] != "status" {
		t.Fatalf("control response = %#v", control)
	}

	client := &http.Client{Transport: &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "unix", socket)
	}}}
	response, err := client.Get("http://unix/internal/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var health map[string]any
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || health["ready"] != true {
		t.Fatalf("health response = %d %#v", response.StatusCode, health)
	}
	_ = response.Body.Close()
	client.Transport.(*http.Transport).CloseIdleConnections()

	snapshot, snapshotStatus, err := Request(context.Background(), socket, []byte(`{"protocol":4,"action":"admin-metrics"}`))
	if err != nil {
		t.Fatal(err)
	}
	if snapshotStatus != http.StatusOK {
		t.Fatalf("snapshot status = %d, body = %s", snapshotStatus, snapshot)
	}
	if !json.Valid(snapshot) {
		t.Fatal("snapshot response is not JSON")
	}

	cancel()
	select {
	case serveErr := <-serverErrors:
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			t.Fatalf("serve error = %v", serveErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not shut down")
	}
}
