package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd/store"
)

func TestPublicationWakesOnCommittedObservationWithoutWaitingForNextPoll(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), time.Hour, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	manager := deployd.NewManager(database, nil, 15*time.Second, time.Now)
	server := deployd.NewServer(manager, deployd.NewMetrics(manager))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); publicationLoop(ctx, server, time.Hour) }()
	defer func() { cancel(); <-done }()
	now := time.Now().UTC()
	sources := map[string]deployd.SourceObservation{}
	for _, name := range []string{"active_link", "systemd", "slot_contracts", "native_observation", "inventory_observation"} {
		sources[name] = deployd.SourceObservation{Up: true, ObservedAt: now, FreshUntil: now.Add(time.Minute)}
	}
	snapshot := deployd.RuntimeSnapshot{ObservedAt: now, FreshUntil: now.Add(time.Minute), ActiveSlot: "blue", ControllerSlot: "blue", Consistent: true, Sources: sources, Slots: map[string]deployd.SlotObservation{"blue": {Slot: "blue", ProcessUp: true, ControllerOwned: true}}}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{}
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	encoded, err = json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	if err := database.SaveObservation(ctx, store.LatestObservationID, store.Observation{ObservedAt: now, FreshUntil: now.Add(time.Minute), Digest: hex.EncodeToString(digest[:]), Payload: payload}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	handler := server.MetricsHandler()
	for {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
		if strings.Contains(response.Body.String(), `agent_vm_deployd_slot_selected{slot="blue"} 1`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("new committed snapshot was not published before the hourly fallback poll")
		}
		time.Sleep(time.Millisecond)
	}
}
