package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/config"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/lifecycle"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/observe"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/protocol"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
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
		if info, statErr := os.Stat(socket); statErr == nil && info.Mode().Perm() == 0o600 {
			break
		}
		select {
		case serveErr := <-serverErrors:
			t.Fatalf("serve exited before creating socket: %v", serveErr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("control socket was not created with mode 0600")
		}
		time.Sleep(5 * time.Millisecond)
	}
	socketInfo, statErr := os.Stat(socket)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if socketInfo.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v", socketInfo.Mode().Perm())
	}
	parentInfo, statErr := os.Stat(root)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if parentInfo.Mode().Perm() != 0o750 {
		t.Fatalf("socket parent mode = %v", parentInfo.Mode().Perm())
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
	_, successStatus, successExit, err := RequestWithExitCode(context.Background(), socket, []byte(`{"protocol":4,"action":"status","session_id":"http-test"}`))
	if err != nil || successStatus != http.StatusOK || successExit != 0 {
		t.Fatalf("validated success status=%d exit=%d err=%v", successStatus, successExit, err)
	}
	invalid, invalidStatus, invalidExit, err := RequestWithExitCode(context.Background(), socket, []byte(`{"protocol":4,"action":"unknown","session_id":"http-test"}`))
	if err != nil || invalidStatus != http.StatusBadRequest || invalidExit != 2 {
		t.Fatalf("validated error status=%d exit=%d err=%v", invalidStatus, invalidExit, err)
	}
	var invalidEnvelope map[string]any
	if err := json.Unmarshal(invalid, &invalidEnvelope); err != nil || invalidEnvelope["ok"] != false {
		t.Fatalf("validated error envelope=%s err=%v", invalid, err)
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

func TestWriteErrorMapsUnreachableWatermarksToServiceUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	writeError(context, protocol.NewError("watermarks_unreachable", "configure reachable CT storage watermarks", nil), nil)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("watermarks error status = %d", recorder.Code)
	}
}
