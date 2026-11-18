package deployd

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/controllerrole"
	deploymentStore "github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd/store"
)

func TestQuiesceHistoricalUnknownIsNotRunningWork(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprint(pending), func(t *testing.T) {
			root := t.TempDir()
			cfg := Defaults()
			cfg.ReleaseRoot = root
			installLayoutFixture(t, root, "blue", false)
			cfg.BlueSocket = filepath.Join(root, "app.sock")
			pid := os.Getpid()
			script := fmt.Sprintf("#!/bin/sh\ncat <<'PROPERTIES'\nActiveState=active\nSubState=running\nMainPID=%d\nFragmentPath=/etc/systemd/system/agent-vm-native@.service\nControlGroup=/system.slice/agent-vm-native@blue.service\nExecStart={ argv[]=%s/slots/blue/bin/agent-vm-control serve --config /etc/agent-vm-native/slots/blue/config.json --manifest %s/slots/blue/release-manifest.json ; }\nPROPERTIES\n", pid, root, root)
			if err := os.WriteFile(filepath.Join(root, "systemctl"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", root+":"+os.Getenv("PATH"))
			listener, err := net.Listen("unix", cfg.BlueSocket)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/internal/v1/deployment-observation" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if r.URL.Path == "/internal/v1/health" {
					json.NewEncoder(w).Encode(map[string]any{"ok": true, "protocol": 4, "process_ready": true, "ready": false, "controller_owned": false, "traffic_accepting": false, "process_id": pid, "process_start_time": controllerrole.ProcessStartToken(pid)})
					return
				}
				var request map[string]any
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				result := map[string]any{"draining": true}
				if request["action"] == "admin-metrics" {
					operations := []any{}
					if pending {
						operations = append(operations, map[string]any{"operation_id": "still-running"})
					}
					result = map[string]any{"pending_operations": operations, "historical_unknown_operations": []any{map[string]any{"operation_id": "superseded"}}, "in_flight_operations": 0}
				}
				json.NewEncoder(w).Encode(map[string]any{"ok": true, "protocol": 4, "action": request["action"], "result": result})
			}))
			server.Listener.Close()
			server.Listener = listener
			server.Start()
			defer server.Close()
			runtime := NewHostRuntime(cfg)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err = runtime.Quiesce(ctx, "blue")
			if (err != nil) != pending {
				t.Fatalf("pending=%t quiesce=%v", pending, err)
			}
			if !pending && runtime.quiesced["blue"].Process.PID != int64(pid) {
				t.Fatal("process ownership was not verified")
			}
		})
	}
}

func TestRealSystemdRollbackCrashBudget(t *testing.T) {
	if os.Getenv("AGENT_VM_ROLLBACK_CRASH_TEST") != "1" {
		t.Skip("owned systemd acceptance only")
	}
	root, err := os.MkdirTemp("/var/lib/agent-vm-native", "rollback-crash-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	installLayoutFixture(t, root, "blue", true)
	// The verified ELF header has no executable program: systemd must observe an actual startup crash.
	extra := "/etc/systemd/system/agent-vm-native@blue.service.d/20-crash-test.conf"
	if err := os.WriteFile(extra, []byte("[Service]\nExecStartPre=\nExecStartPost=\n"), 0644); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = exec.Command("systemctl", "stop", "agent-vm-native@blue.service").Run()
		_ = os.Remove(extra)
		_ = exec.Command("systemctl", "daemon-reload").Run()
	}()
	// Earlier acceptance stages reuse this unit; its start-rate history must
	// not reject the deliberately crashing executable before it runs.
	if output, err := exec.Command("systemctl", "reset-failed", "agent-vm-native@blue.service").CombinedOutput(); err != nil {
		t.Fatalf("reset test unit start-rate history: %s: %v", output, err)
	}
	cfg := Defaults()
	cfg.ReleaseRoot = root
	cfg.BlueSocket = filepath.Join(root, "missing.sock")
	runtime := NewHostRuntime(cfg)
	path := filepath.Join(root, "attempts.sqlite3")
	db, err := deploymentStore.Open(path, time.Hour, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.Create(ctx, deploymentStore.Transaction{ID: "crash", TargetSlot: "green", PreviousSlot: "blue", Bundle: "verified", Generation: 1, Phase: "rollback", State: "active"}, nil); err != nil {
		t.Fatal(err)
	}
	claimed := 0
	claim := func() error {
		if claimed == 1 { // Reopen the durable journal between first start and retry.
			if err := db.Close(); err != nil {
				return err
			}
			db, err = deploymentStore.Open(path, time.Hour, time.Now)
			if err != nil {
				return err
			}
		}
		if err := db.ConsumeRollbackStart(ctx, "crash"); err != nil {
			return err
		}
		claimed++
		return nil
	}
	ctx = context.WithValue(ctx, rollbackStartKey{}, claim)
	if err := runtime.startRollbackPrevious(ctx, "blue"); err == nil || !strings.Contains(err.Error(), "three starts") {
		t.Fatalf("rollback result: %v", err)
	}
	if claimed != 3 {
		t.Fatalf("starts=%d, want 3", claimed)
	}
	if err := runtime.startRollbackPrevious(ctx, "blue"); err == nil || claimed != 3 {
		t.Fatalf("exhausted retry budget restarted old slot: %d %v", claimed, err)
	}
	output, err := exec.Command("systemctl", "show", "agent-vm-native@blue.service", "--property=Restart", "--property=MainPID").Output()
	if err != nil || !strings.Contains(string(output), "Restart=no") || !strings.Contains(string(output), "MainPID=0") {
		t.Fatalf("crashing slot not stopped: %s %v", output, err)
	}
	executable := filepath.Join(root, "slots", "blue", "bin", "agent-vm-server")
	journal, err := exec.Command("journalctl", "-u", "agent-vm-native@blue.service", "--no-pager", "--output=cat", "--grep="+executable).Output()
	if count := strings.Count(string(journal), "Failed to execute "+executable); err != nil || count != 3 {
		t.Fatalf("observed %d real execution failures, want three: %s %v", count, journal, err)
	}
}
