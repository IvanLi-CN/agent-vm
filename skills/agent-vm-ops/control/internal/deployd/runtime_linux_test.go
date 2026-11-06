package deployd

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/controllerrole"
)

func TestQuiesceHistoricalUnknownIsNotRunningWork(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprint(pending), func(t *testing.T) {
			root := t.TempDir()
			cfg := Defaults()
			cfg.ReleaseRoot = root
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
