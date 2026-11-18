//go:build linux

package install

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd"
)

func TestNativeAccountMigrationCandidate(t *testing.T) {
	root := os.Getenv("AGENT_VM_NATIVE_ACCOUNT_FIXTURE")
	config := os.Getenv("AGENT_VM_NATIVE_ACCOUNT_CONFIG")
	if root == "" || config == "" {
		t.Skip("requires a session-owned Linux fixture with a running historical blue-green installation")
	}
	if os.Geteuid() != 0 {
		t.Fatal("fixture requires local root")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	input := Inputs{Config: config, Bundle: filepath.Join(root, "product.tar"), Materials: filepath.Join(root, "materials")}
	report, err := PreflightAccountMigration(ctx, input)
	if err != nil {
		t.Fatalf("preflight: %v (%v)", err, report.Blockers)
	}
	product := filepath.Join(t.TempDir(), "product")
	file, err := os.Open(input.Bundle)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := Extract(file, product, 256<<20); err != nil {
		t.Fatal(err)
	}
	client := deployd.Client{Socket: deployd.Defaults().Socket, Timeout: 8 * time.Minute}
	receipt, err := prepareAccountCandidate(ctx, input, product, report, client)
	t.Logf("transaction=%s phase=%s", receipt.ID, receipt.Action)
	if err != nil {
		t.Fatal(err)
	}
	assertNativeSSHRequest(t, ctx)
	handoff, err := activateAccountCandidate(ctx, client, receipt, report)
	if err != nil {
		t.Fatal(err)
	}
	if err := handoffDeploymentUnit(ctx, handoff, input); err != nil {
		t.Fatal(err)
	}
	assertNativeSSHRequest(t, ctx)
	t.Logf("active=%s deployment account adopted the controller", handoff.Lease.Slot)
}

func TestNativeRequestRelayRejectUnknownOwner(t *testing.T) {
	if os.Getenv("AGENT_VM_NATIVE_ACCOUNT_FIXTURE") == "" || os.Geteuid() != 0 {
		t.Skip("requires the session-owned Linux account migration fixture")
	}
	active, err := deployd.EffectiveActivePath(deployd.Defaults().ActivePath)
	if err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(active)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	assertNativeSSHRejectsUnknownOwner(t, ctx, filepath.Base(target))
	assertNativeSSHRequest(t, ctx)
}

func assertNativeSSHRejectsUnknownOwner(t *testing.T, ctx context.Context, slot string) {
	t.Helper()
	path := filepath.Join("/run/agent-vm-native", slot, "app.sock")
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	uid, gid := info.Sys().(*syscall.Stat_t).Uid, info.Sys().(*syscall.Stat_t).Gid
	if err := os.Chown(path, 65534, int(gid)); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chown(path, int(uid), int(gid)); err != nil {
			t.Error(err)
		}
	}()
	request := exec.CommandContext(ctx, "/opt/agent-vm-native/bin/agent-vm-server", "request", "--socket", deployd.RequestRelaySocket)
	request.Stdin = strings.NewReader(`{"protocol":4,"action":"status","session_id":"native-account-ssh","generation":0}`)
	output, err := request.CombinedOutput()
	if err == nil || !bytes.Contains(output, []byte("unsupported owner")) {
		t.Fatalf("unknown socket identity was not rejected: %v %s", err, output)
	}
}

func assertNativeSSHRequest(t *testing.T, ctx context.Context) {
	t.Helper()
	request := exec.CommandContext(ctx, "/opt/agent-vm-native/bin/agent-vm-server", "request", "--socket", deployd.RequestRelaySocket)
	request.Stdin = bytes.NewBufferString(`{"protocol":4,"action":"status","session_id":"native-account-ssh","generation":0}`)
	output, err := request.CombinedOutput()
	var response struct {
		OK bool `json:"ok"`
	}
	if err != nil || json.Unmarshal(output, &response) != nil || !response.OK {
		t.Fatalf("historical root SSH request failed: %v %s", err, output)
	}
}
