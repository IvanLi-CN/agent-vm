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
	"strings"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/controllerrole"
)

// slotRestartFixture serves one live slot health contract plus a fake systemctl
// reporting the same process, so the controller pin path runs without a unit.
type slotRestartFixture struct {
	root    string
	socket  string
	release string
	pid     int
	token   string
	owned   bool
	lease   controllerrole.Lease
	runtime *HostRuntime
}

func newSlotRestartFixture(t *testing.T) *slotRestartFixture {
	t.Helper()
	root := t.TempDir()
	release := strings.Repeat("a", 40)
	installLayoutFixture(t, root, "blue", true)
	slotRoot := filepath.Join(root, "slots", "blue")
	if err := os.Chmod(slotRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	token := controllerrole.ProcessStartToken(pid)
	if token == "" {
		t.Skip("process start token is unavailable")
	}
	socket := filepath.Join(root, "blue.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &slotRestartFixture{root: root, socket: socket, release: release, pid: pid, token: token}
	if err := os.Symlink(socket, filepath.Join(slotRoot, "app.sock")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(slotRoot, filepath.Join(root, "active")); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/health" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body := map[string]any{
			"ok": true, "protocol": 4, "process_ready": true, "ready": true,
			"controller_owned": fixture.owned, "controller_lease_renewal": true,
			"traffic_accepting": false, "process_id": fixture.pid, "process_start_time": fixture.token,
			"release_id": fixture.release, "source_revision": fixture.release,
		}
		if fixture.owned {
			body["lease"] = fixture.lease
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	fixture.writeSystemctl(t)
	cfg := Defaults()
	cfg.ReleaseRoot = root
	cfg.ActivePath = filepath.Join(root, "active")
	cfg.BlueSocket = socket
	fixture.runtime = NewHostRuntime(cfg)
	return fixture
}

// writeSystemctl reports this slot as an active unit running the test process
// and the other slot as a cleanly stopped unit.
func (fixture *slotRestartFixture) writeSystemctl(t *testing.T) {
	t.Helper()
	binary := filepath.Join(fixture.root, "slots", "blue", "bin", "agent-vm-server")
	config := "/etc/agent-vm-native/slots/blue/config.json"
	manifest := filepath.Join(fixture.root, "slots", "blue", "release-manifest.json")
	script := fmt.Sprintf(`#!/bin/sh
unit=""
for argument in "$@"; do
  case "$argument" in
    agent-vm-native@*) unit="$argument" ;;
  esac
done
case "$unit" in
  agent-vm-native@blue.service)
    cat <<'PROPERTIES'
LoadState=loaded
ActiveState=active
SubState=running
MainPID=%d
ControlGroup=/system.slice/agent-vm-native.slice/agent-vm-native@blue.service
Delegate=no
Slice=system-agent\x2dvm\x2dnative.slice
FragmentPath=/etc/systemd/system/agent-vm-native@.service
ExecStart={ argv[]=%s serve --config %s --manifest %s ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }
PROPERTIES
    ;;
  *)
    cat <<'PROPERTIES'
LoadState=loaded
ActiveState=inactive
SubState=dead
MainPID=0
ControlGroup=
FragmentPath=/etc/systemd/system/agent-vm-native@.service
PROPERTIES
    ;;
esac
`, fixture.pid, binary, config, manifest)
	if err := os.WriteFile(filepath.Join(fixture.root, "systemctl"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fixture.root+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A slot restart replaces the process behind the same socket. The stale pin is
// forgotten rather than reported as an error: an error fences both roles on
// every heartbeat, and only a successful observation can refresh a pin, so the
// host would never recover without restarting deployd.
func TestSlotRestartDropsStalePinInsteadOfFencing(t *testing.T) {
	fixture := newSlotRestartFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fixture.runtime.stagedProcess["blue"] = processIdentity{PID: int64(fixture.pid) + 1, StartTime: "1"}
	if _, owned, err := fixture.runtime.CurrentOwnedControllerLease(ctx); err != nil || owned {
		t.Fatalf("stale pin produced owned=%t err=%v", owned, err)
	}
	if _, present := fixture.runtime.stagedProcess["blue"]; present {
		t.Fatal("stale pin was retained")
	}

	// The recovery path is reachable now: the verified release identity,
	// systemd process and socket link re-pin the new process.
	target := SlotObservation{Slot: "blue", ReleaseID: fixture.release, SourceRevision: fixture.release, StateCapabilities: []string{"reads@1"}}
	if err := fixture.runtime.PrepareControllerRenewal(ctx, target); err != nil {
		t.Fatalf("re-pin after slot restart failed: %v", err)
	}
	if pinned, present := fixture.runtime.stagedProcess["blue"]; !present || pinned.PID != int64(fixture.pid) || pinned.StartTime != fixture.token {
		t.Fatalf("re-pinned process = %+v", pinned)
	}
}

// A pin that still matches keeps returning the live lease, so the fix does not
// weaken normal renewal.
func TestMatchingPinStillReturnsLiveLease(t *testing.T) {
	fixture := newSlotRestartFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture.runtime.stagedProcess["blue"] = processIdentity{PID: int64(fixture.pid), StartTime: fixture.token}
	fixture.owned = true
	fixture.lease = controllerrole.Lease{
		Slot: "blue", Term: 3, LeaseID: "lease-live", Nonce: "nonce-live", Generation: 1,
		Expiry: time.Now().Add(time.Minute), OwnerPID: os.Getpid(), OwnerStartTime: fixture.token,
	}
	observed, owned, err := fixture.runtime.CurrentOwnedControllerLease(ctx)
	if err != nil || !owned || !sameLeaseAuthority(observed, fixture.lease) {
		t.Fatalf("live pin lost authority: owned=%t err=%v lease=%+v", owned, err, observed)
	}
}
