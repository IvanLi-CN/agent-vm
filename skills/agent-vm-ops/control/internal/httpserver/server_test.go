package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/config"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/controllerrole"
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
	before, err := store.StateEpoch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	combined, err := client.Get("http://unix/internal/v1/deployment-observation")
	if err != nil {
		t.Fatal(err)
	}
	var combinedEnvelope struct {
		OK       bool                      `json:"ok"`
		Protocol int                       `json:"protocol"`
		Result   map[string]map[string]any `json:"result"`
	}
	err = json.NewDecoder(combined.Body).Decode(&combinedEnvelope)
	combined.Body.Close()
	if err != nil || combined.StatusCode != http.StatusOK || !combinedEnvelope.OK || combinedEnvelope.Protocol != 4 {
		t.Fatalf("combined observation: status=%d value=%+v err=%v", combined.StatusCode, combinedEnvelope, err)
	}
	if len(combinedEnvelope.Result) != 3 || combinedEnvelope.Result["capacity"]["observation_epoch"] != before || combinedEnvelope.Result["health"]["state_epoch"] != before || combinedEnvelope.Result["inventory"]["observation_consistent"] != true {
		t.Fatalf("combined state view was not coherent: %+v", combinedEnvelope.Result)
	}
	if after, err := store.StateEpoch(context.Background()); err != nil || after != before {
		t.Fatalf("observation mutated state: %s -> %s, %v", before, after, err)
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

func TestTrafficRoleStartsFencedAndRequiresControllerLease(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := lifecycle.New(store, config.Defaults(), lifecycle.Options{})
	gate := controllerrole.New("blue", time.Now)
	server := NewWithRole(service, nil, nil, gate)

	request := httptest.NewRequest(http.MethodPost, "/internal/v1/traffic-role", strings.NewReader(`{"action":"enable"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict || service.TrafficAccepting() {
		t.Fatalf("traffic enable without lease status=%d accepting=%v", recorder.Code, service.TrafficAccepting())
	}

	lease := controllerrole.Lease{Slot: "blue", Term: 1, LeaseID: "lease", Nonce: "nonce", Expiry: time.Now().Add(time.Minute)}
	if err := gate.Grant(lease); err != nil {
		t.Fatal(err)
	}
	service.MarkReady()
	request = httptest.NewRequest(http.MethodPost, "/internal/v1/traffic-role", strings.NewReader(`{"action":"enable"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !service.TrafficAccepting() {
		t.Fatalf("traffic enable with lease status=%d accepting=%v", recorder.Code, service.TrafficAccepting())
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

func TestControllerHealthNegotiatesRenewalWithoutCancelingAuthority(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := lifecycle.New(store, config.Defaults(), lifecycle.Options{})
	gate := controllerrole.New("blue", time.Now)
	server := NewWithRole(service, nil, nil, gate)
	lease := controllerrole.Lease{Slot: "blue", Term: 1, LeaseID: "lease", Nonce: "nonce", Generation: 1, Expiry: time.Now().UTC().Add(time.Minute)}
	grant := func() {
		body, err := json.Marshal(map[string]any{"action": "grant", "lease": lease})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/internal/v1/controller-role", bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("grant status=%d body=%s", response.Code, response.Body.String())
		}
	}
	grant()
	changed := gate.Changes()
	lease.Expiry = lease.Expiry.Add(time.Minute)
	grant()
	select {
	case <-changed:
		t.Fatal("HTTP renewal canceled live authority")
	default:
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/internal/v1/health", nil))
	var health struct {
		Renewal  bool                 `json:"controller_lease_renewal"`
		Owned    bool                 `json:"controller_owned"`
		Lease    controllerrole.Lease `json:"lease"`
		Contract struct {
			Name  string `json:"name"`
			Major int64  `json:"major"`
		} `json:"deployment_contract"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if !health.Renewal || !health.Owned || health.Lease != lease || health.Contract.Name != protocol.DeploymentContractName || health.Contract.Major != protocol.DeploymentContractMajor {
		t.Fatalf("health negotiation=%+v", health)
	}
}
