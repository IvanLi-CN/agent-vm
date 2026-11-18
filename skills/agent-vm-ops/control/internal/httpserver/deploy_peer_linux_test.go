package httpserver

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/config"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/controllerrole"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/lifecycle"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

// Run as root inside a disposable Linux guest with both product accounts present.
func TestNativeDeploymentPeerCredentials(t *testing.T) {
	switch os.Getenv("AGENT_VM_TEST_DEPLOY_PEER") {
	case "listen":
		socket := os.Getenv("AGENT_VM_TEST_DEPLOY_SOCKET")
		store, err := state.OpenForTest(filepath.Join(filepath.Dir(socket), "state.sqlite3"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		service := lifecycle.New(store, config.Defaults(), lifecycle.Options{})
		service.MarkReady()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := ServeAppRole(ctx, socket, service, nil, http.NotFoundHandler(), controllerrole.New("blue", time.Now)); err != nil && err != http.ErrServerClosed {
			t.Fatal(err)
		}
		return
	case "1":
	default:
		t.Skip("set AGENT_VM_TEST_DEPLOY_PEER=1 in an isolated Linux guest")
	}
	if os.Getuid() != 0 {
		t.Fatal("guest fixture requires root to launch distinct accounts")
	}
	dir, err := os.MkdirTemp("/tmp", "agent-vm-peer-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	account, err := user.Lookup("agent-vm-server")
	if err != nil {
		t.Fatal(err)
	}
	uid, uidErr := strconv.ParseUint(account.Uid, 10, 32)
	gid, gidErr := strconv.ParseUint(account.Gid, 10, 32)
	if uidErr != nil || gidErr != nil || uid == 0 || gid == 0 {
		t.Fatal("expected a dedicated service UID and GID")
	}
	if err := os.Chown(dir, int(uid), int(gid)); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "slot.sock")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	listener := exec.CommandContext(ctx, binary, "-test.run=^TestNativeDeploymentPeerCredentials$")
	listener.Env = []string{"AGENT_VM_TEST_DEPLOY_PEER=listen", "AGENT_VM_TEST_DEPLOY_SOCKET=" + socket}
	listener.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}}
	listener.Stdout, listener.Stderr = os.Stdout, os.Stderr
	if err := listener.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = listener.Wait() }()
	for {
		if info, err := os.Stat(socket); err == nil && info.Mode().Perm() == 0660 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("slot socket did not start")
		case <-time.After(20 * time.Millisecond):
		}
	}
	// Give every fixture peer file access so the test exercises the UID check.
	for path, mode := range map[string]os.FileMode{dir: 0755, socket: 0666} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ user, path, body, status, fragment string }{
		{"agent-vm-deploy", "health", "", "200", `"ready":true`},
		{"agent-vm-deploy", "control", `{"protocol":4,"action":"destroy","session_id":"fixture","generation":1,"operation_id":"test"}`, "403", "peer_unauthorized"},
		{"agent-vm-deploy", "traffic-role", `{"action":"enable"}`, "409", "controller"},
		{"nobody", "health", "", "403", "peer_unauthorized"},
		{"root", "health", "", "200", `"ready":true`},
	} {
		args := []string{"-u", tc.user, "--", "curl", "--noproxy", "*", "-sS", "-w", "\n%{http_code}", "--unix-socket", socket}
		if tc.body != "" {
			args = append(args, "-H", "Content-Type: application/json", "--data-binary", tc.body)
		}
		out, err := exec.CommandContext(ctx, "runuser", append(args, "http://localhost/internal/v1/"+tc.path)...).CombinedOutput()
		if err != nil || !strings.HasSuffix(string(out), "\n"+tc.status) || !strings.Contains(string(out), tc.fragment) {
			t.Errorf("%s %s: %v: %s", tc.user, tc.path, err, out)
		}
	}
}
