package deployd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/controllerrole"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd/store"
)

func TestServeUDSRefusesLiveAndReplacesStaleSocket(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "deployd-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	socket := filepath.Join(directory, "deployd.sock")
	live, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := ServeUDS(context.Background(), socket, http.NotFoundHandler()); err == nil {
		t.Fatal("live deployd socket was replaced")
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errorsCh := make(chan error, 1)
	go func() { errorsCh <- ServeUDS(ctx, socket, http.NotFoundHandler()) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, statErr := os.Stat(socket); statErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("deployd did not replace stale socket")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case serveErr := <-errorsCh:
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			t.Fatalf("serve error = %v", serveErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("deployd did not stop")
	}
}

type acceptanceRuntime struct {
	mu             sync.Mutex
	active         string
	slots          map[string]SlotObservation
	draining       map[string]bool
	traffic        map[string]bool
	uncertain      bool
	stageUncertain bool
}

func newAcceptanceRuntime() *acceptanceRuntime {
	return &acceptanceRuntime{
		active: "blue",
		slots: map[string]SlotObservation{
			"blue":  {Slot: "blue", ProcessUp: true, ProcessReady: true, Ready: true, ControllerOwned: true},
			"green": {Slot: "green", ProcessUp: true, ProcessReady: true, Ready: true},
		},
		draining: map[string]bool{},
		traffic:  map[string]bool{"blue": true},
	}
}

func (runtime *acceptanceRuntime) Observe(context.Context) (RuntimeSnapshot, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	now := time.Now().UTC()
	slots := make(map[string]SlotObservation, len(runtime.slots))
	for slot, value := range runtime.slots {
		value.TrafficAccepting = runtime.traffic[slot] && !runtime.draining[slot]
		value.ReleaseID = "release-" + slot
		value.SourceRevision = "revision-1"
		value.StateEpoch = "epoch-1"
		value.StateCapabilities = []string{"reads@1", "writes@1"}
		value.CountersKnown = true
		value.ObservedUnix = now.Unix()
		slots[slot] = value
	}
	controllerSlot := "none"
	for slot, value := range slots {
		if value.ControllerOwned {
			if controllerSlot != "none" {
				controllerSlot = "unknown"
				break
			}
			controllerSlot = slot
		}
	}
	sources := map[string]SourceObservation{}
	for _, source := range []string{"active_link", "systemd", "slot_contracts", "native_observation", "inventory_observation"} {
		sources[source] = SourceObservation{Up: true, ObservedAt: now, FreshUntil: now.Add(30 * time.Second)}
	}
	return RuntimeSnapshot{
		ObservedAt:     now,
		FreshUntil:     now.Add(30 * time.Second),
		ActiveSlot:     runtime.active,
		ControllerSlot: controllerSlot,
		Consistent:     true,
		Slots:          slots,
		Sources:        sources,
	}, nil
}

func (runtime *acceptanceRuntime) Stage(_ context.Context, slot, _ string) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.stageUncertain {
		return &UncertainError{Cause: errors.New("acceptance stage response was interrupted")}
	}
	value := runtime.slots[slot]
	value.Slot = slot
	value.ProcessUp = true
	value.ProcessReady = true
	value.Ready = true
	runtime.slots[slot] = value
	runtime.draining[slot] = false
	return nil
}

func (runtime *acceptanceRuntime) Verify(context.Context, string, string) error { return nil }

func (runtime *acceptanceRuntime) VerifySlotIdentity(context.Context, string, string, string, []string) error {
	return nil
}

func (runtime *acceptanceRuntime) GrantController(_ context.Context, slot string, _ controllerrole.Lease) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	value := runtime.slots[slot]
	value.ControllerOwned = true
	runtime.slots[slot] = value
	return nil
}

func (runtime *acceptanceRuntime) RevokeController(_ context.Context, slot string, _ uint64) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if value, ok := runtime.slots[slot]; ok {
		value.ControllerOwned = false
		runtime.slots[slot] = value
	}
	return nil
}

func (runtime *acceptanceRuntime) FenceController(_ context.Context, slot string) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if value, ok := runtime.slots[slot]; ok {
		value.ControllerOwned = false
		runtime.slots[slot] = value
	}
	runtime.traffic[slot] = false
	return nil
}

func (runtime *acceptanceRuntime) EnableTraffic(_ context.Context, slot string) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.traffic[slot] = true
	return nil
}

func (runtime *acceptanceRuntime) DisableTraffic(_ context.Context, slot string) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.traffic[slot] = false
	return nil
}

func (runtime *acceptanceRuntime) SwitchActive(_ context.Context, slot string) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.uncertain {
		return &UncertainError{Cause: errors.New("acceptance switch response was interrupted")}
	}
	runtime.active = slot
	return nil
}

func (runtime *acceptanceRuntime) Drain(_ context.Context, slot string) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.draining[slot] = true
	return nil
}

func (runtime *acceptanceRuntime) ExitDrain(_ context.Context, slot string) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.draining[slot] = false
	return nil
}

func (runtime *acceptanceRuntime) Quiesce(context.Context, string) error { return nil }

func (runtime *acceptanceRuntime) Stop(_ context.Context, slot string) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	value := runtime.slots[slot]
	value.ProcessUp = false
	value.ProcessReady = false
	value.Ready = false
	value.ControllerOwned = false
	runtime.slots[slot] = value
	return nil
}

func (runtime *acceptanceRuntime) Rollback(_ context.Context, target, previous string, _ controllerrole.Lease) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	previousValue := runtime.slots[previous]
	previousValue.ProcessUp = true
	previousValue.ProcessReady = true
	previousValue.Ready = true
	previousValue.ControllerOwned = true
	runtime.slots[previous] = previousValue
	targetValue := runtime.slots[target]
	targetValue.ProcessUp = false
	targetValue.ProcessReady = false
	targetValue.Ready = false
	targetValue.ControllerOwned = false
	runtime.slots[target] = targetValue
	runtime.draining[target] = true
	runtime.draining[previous] = false
	runtime.active = previous
	runtime.traffic[previous] = false
	runtime.traffic[target] = false
	return nil
}

func TestDeploydAgentVMAcceptance(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	runtime := newAcceptanceRuntime()
	manager := NewManager(database, runtime, 15*time.Second, time.Now)
	metrics := NewMetrics(manager)
	server := NewServer(manager, metrics)
	socketDir, err := os.MkdirTemp("/tmp", "d-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketDir)
	socket := filepath.Join(socketDir, "d.sock")
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- ServeUDS(ctx, socket, server.Handler()) }()
	waitForSocket(t, socket, serveErrors)
	client := Client{Socket: socket, Timeout: 10 * time.Second}
	metricsServer := httptest.NewServer(server.MetricsHandler())
	defer metricsServer.Close()

	if err := server.Sample(ctx); err != nil {
		t.Fatal(err)
	}
	initialMetrics := readMetrics(t, metricsServer.URL)
	assertMetric(t, initialMetrics, `agent_vm_deployd_slot_process_up{slot="blue"} 1`)
	assertMetric(t, initialMetrics, `agent_vm_deployd_slot_process_up{slot="green"} 1`)
	assertMetric(t, initialMetrics, `agent_vm_deployd_slot_controller_owned{slot="blue"} 1`)
	assertMetric(t, initialMetrics, `agent_vm_deployd_slot_controller_owned{slot="green"} 0`)
	assertMetric(t, initialMetrics, "agent_vm_deployd_snapshot_fresh 1")
	if strings.Contains(initialMetrics, "agent_vm_deployd_slot_controller_owned{slot=\"blue\"} 1\nagent_vm_deployd_slot_controller_owned{slot=\"blue\"} 1") {
		t.Fatal("duplicate controller series were exposed")
	}
	initialStatus := callDeploydJSON(t, client, http.MethodGet, "/v1/status", nil)
	initialObservation, ok := initialStatus["observation"].(map[string]any)
	if !ok || initialObservation["controller_slot"] != "blue" {
		t.Fatalf("initial controller slot = %#v", initialStatus["observation"])
	}

	plan := callDeploydJSON(t, client, http.MethodPost, "/v1/plan", map[string]any{"target_slot": "green", "bundle": "/verified/green.json"})
	if plan["target_slot"] != "green" {
		t.Fatalf("plan target = %#v", plan["target_slot"])
	}
	staged := callDeploydJSON(t, client, http.MethodPost, "/v1/stage", map[string]any{"target_slot": "green", "bundle": "/verified/green.json"})
	transactionID, _ := staged["transaction_id"].(string)
	if transactionID == "" {
		t.Fatalf("stage response did not contain a transaction ID: %#v", staged)
	}
	callDeploydJSON(t, client, http.MethodPost, "/v1/verify", map[string]any{"transaction_id": transactionID})
	if err := server.Sample(ctx); err != nil {
		t.Fatal(err)
	}
	dualMetrics := readMetrics(t, metricsServer.URL)
	assertMetric(t, dualMetrics, `agent_vm_deployd_slot_process_up{slot="blue"} 1`)
	assertMetric(t, dualMetrics, `agent_vm_deployd_slot_process_up{slot="green"} 1`)
	assertMetric(t, dualMetrics, `agent_vm_deployd_slot_controller_owned{slot="blue"} 1`)
	assertMetric(t, dualMetrics, `agent_vm_deployd_slot_controller_owned{slot="green"} 0`)

	callDeploydJSON(t, client, http.MethodPost, "/v1/activate", map[string]any{"transaction_id": transactionID})
	if err := server.Sample(ctx); err != nil {
		t.Fatal(err)
	}
	activeMetrics := readMetrics(t, metricsServer.URL)
	assertMetric(t, activeMetrics, `agent_vm_deployd_active_slot{slot="green"} 1`)
	assertMetric(t, activeMetrics, `agent_vm_deployd_slot_process_up{slot="blue"} 0`)
	assertMetric(t, activeMetrics, `agent_vm_deployd_slot_process_up{slot="green"} 1`)
	assertMetric(t, activeMetrics, `agent_vm_deployd_slot_controller_owned{slot="blue"} 0`)
	assertMetric(t, activeMetrics, `agent_vm_deployd_slot_controller_owned{slot="green"} 1`)
	runtime.mu.Lock()
	if !runtime.draining["blue"] {
		runtime.mu.Unlock()
		t.Fatal("successful activation reopened the old slot before it was stopped")
	}
	runtime.mu.Unlock()

	callDeploydJSON(t, client, http.MethodPost, "/v1/rollback", map[string]any{"transaction_id": transactionID})
	if err := server.Sample(ctx); err != nil {
		t.Fatal(err)
	}
	rollbackMetrics := readMetrics(t, metricsServer.URL)
	assertMetric(t, rollbackMetrics, `agent_vm_deployd_active_slot{slot="blue"} 1`)
	assertMetric(t, rollbackMetrics, `agent_vm_deployd_slot_process_up{slot="blue"} 1`)
	assertMetric(t, rollbackMetrics, `agent_vm_deployd_slot_process_up{slot="green"} 0`)

	runtime.mu.Lock()
	runtime.uncertain = false
	runtime.mu.Unlock()
	uncertainStage := callDeploydJSON(t, client, http.MethodPost, "/v1/stage", map[string]any{"target_slot": "green", "bundle": "/verified/green-uncertain.json"})
	uncertainID, _ := uncertainStage["transaction_id"].(string)
	callDeploydJSON(t, client, http.MethodPost, "/v1/verify", map[string]any{"transaction_id": uncertainID})
	runtime.mu.Lock()
	runtime.uncertain = true
	runtime.mu.Unlock()
	if _, status, err := client.Call(ctx, http.MethodPost, "/v1/activate", map[string]any{"transaction_id": uncertainID}); err == nil || status != http.StatusConflict {
		t.Fatalf("uncertain activation status=%d err=%v", status, err)
	}
	uncertainTransaction, err := database.Get(ctx, uncertainID)
	if err != nil || uncertainTransaction == nil || uncertainTransaction.State != "needs_recovery" {
		t.Fatalf("uncertain transaction = %#v err=%v", uncertainTransaction, err)
	}
	runtime.mu.Lock()
	for slot, value := range runtime.slots {
		if value.ControllerOwned {
			t.Fatalf("slot %s retained controller ownership after uncertain switch", slot)
		}
	}
	runtime.uncertain = false
	runtime.mu.Unlock()
	callDeploydJSON(t, client, http.MethodPost, "/v1/recover", map[string]any{"transaction_id": uncertainID, "action": "rollback"})

	runtime.mu.Lock()
	runtime.stageUncertain = true
	runtime.mu.Unlock()
	uncertainStagePayload, uncertainStageStatus, uncertainStageErr := client.Call(ctx, http.MethodPost, "/v1/stage", map[string]any{"target_slot": "green", "bundle": "/verified/green-stage-uncertain.json"})
	if uncertainStageErr == nil || uncertainStageStatus != http.StatusConflict {
		t.Fatalf("uncertain stage status=%d err=%v payload=%s", uncertainStageStatus, uncertainStageErr, uncertainStagePayload)
	}
	var uncertainStageEnvelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(uncertainStagePayload, &uncertainStageEnvelope); err != nil {
		t.Fatal(err)
	}
	if uncertainStageEnvelope.Error.Code != "state_unverified" {
		t.Fatalf("uncertain stage error code = %q, want state_unverified", uncertainStageEnvelope.Error.Code)
	}
	runtime.mu.Lock()
	runtime.stageUncertain = false
	runtime.mu.Unlock()
	activeTransactions, err := database.ListActive(ctx)
	if err != nil || len(activeTransactions) != 1 || activeTransactions[0].State != "needs_recovery" {
		t.Fatalf("uncertain stage transaction = %#v err=%v", activeTransactions, err)
	}
	callDeploydJSON(t, client, http.MethodPost, "/v1/recover", map[string]any{"transaction_id": activeTransactions[0].ID, "action": "rollback"})

	restartTransaction := store.Transaction{
		ID: "tx-restart-acceptance", TargetSlot: "green", PreviousSlot: "blue", Bundle: "/verified/restart.json",
		Phase: PhaseActiveSwitch, State: "active", Generation: time.Now().UnixNano(), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := database.Create(ctx, restartTransaction, nil); err != nil {
		t.Fatal(err)
	}
	managerAfterRestart := NewManager(database, runtime, 15*time.Second, time.Now)
	if err := managerAfterRestart.RecoverOnStart(ctx); err == nil || !errors.Is(err, ErrUncertain) {
		t.Fatalf("restart recovery error = %v", err)
	}
	if err := managerAfterRestart.MaintainController(ctx); err == nil {
		t.Fatal("controller maintenance succeeded while recovery evidence was unresolved")
	}
	for slot, value := range runtime.slots {
		if value.ControllerOwned {
			t.Fatalf("slot %s retained controller ownership after restart fencing", slot)
		}
	}
	if response, status, err := client.Call(ctx, http.MethodGet, "/v1/transactions/tx-restart-acceptance", nil); err != nil || status != http.StatusOK || len(response) == 0 {
		t.Fatalf("restart transaction query status=%d err=%v response=%s", status, err, response)
	}

	cancel()
	select {
	case <-serveErrors:
	case <-time.After(2 * time.Second):
		t.Fatal("Deployd UDS server did not stop")
	}
}

func waitForSocket(t *testing.T, path string, errorsCh <-chan error) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-errorsCh:
			t.Fatalf("Deployd UDS server exited before listening: %v", err)
		default:
		}
		if info, err := os.Stat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Deployd UDS socket did not appear: %s", path)
}

func callDeploydJSON(t *testing.T, client Client, method, path string, request any) map[string]any {
	t.Helper()
	payload, status, err := client.Call(context.Background(), method, path, request)
	if err != nil || status != http.StatusOK {
		t.Fatalf("Deployd API %s %s status=%d err=%v payload=%s", method, path, status, err, payload)
	}
	var envelope struct {
		Value map[string]any `json:"value"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Value
}

func readMetrics(t *testing.T, endpoint string) string {
	t.Helper()
	response, err := http.Get(endpoint + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func assertMetric(t *testing.T, text, expected string) {
	t.Helper()
	if !strings.Contains(text, expected+"\n") && !strings.HasSuffix(text, expected) {
		t.Fatalf("metrics missing %q:\n%s", expected, text)
	}
}

var _ Runtime = (*acceptanceRuntime)(nil)
