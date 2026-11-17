package deployd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/protocol"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func installLayoutFixture(t *testing.T, root, slot string, consolidated bool) string {
	t.Helper()
	base := filepath.Join(root, "slots", slot)
	binary := "agent-vm-control"
	binaries := []string{"agent-vm-control", "agent-vm-deployd", "agent-vm-deploy"}
	if consolidated {
		binary = "agent-vm-server"
		binaries = []string{binary, "agent-vm-deploy"}
	}
	artifacts := []any{}
	add := func(name, path, role string, mode int, data []byte) {
		destination := filepath.Join(base, path)
		if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(destination, data, os.FileMode(mode)); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(data)
		artifacts = append(artifacts, map[string]any{"name": name, "path": path, "role": role, "mode": mode, "sha256": hex.EncodeToString(digest[:])})
	}
	for _, name := range binaries {
		data := make([]byte, 20)
		copy(data, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
		data[18] = 62
		if runtime.GOARCH == "arm64" {
			data[18] = 183
		}
		add(name, "bin/"+name, "control", 0755, data)
	}
	for _, name := range []string{"agent-vm-native.service", "agent-vm-native@.service", "agent-vm-deployd.service"} {
		add("unit:"+name, "systemd/"+name, "unit", 0644, []byte("fixture"))
	}
	actions := map[string]any{}
	for name := range protocol.Actions {
		actions[name] = map[string]any{"request": map[string]any{"schema": 1, "semantic_revision": 1}, "response": map[string]any{"schema": 1, "semantic_revision": 1}, "client_to_control": map[string]any{"schema_min": 1, "schema_max": 1, "semantic_min": 1, "semantic_max": 1}, "control_to_client": map[string]any{"schema_min": 1, "schema_max": 1, "semantic_min": 1, "semantic_max": 1}}
	}
	caps := []string{"legacy-agent-vm-core@1", "managed-image-catalog@1", "management-operation-audit@1", "oidc-return-path@1", "principal-identity@1", "session-image-binding@1", "token-revocation@1"}
	revision := strings.Repeat("a", 40)
	manifest := map[string]any{"format": 4, "version": "1.0.0", "release_id": revision, "source_revision": revision, "protocol": 4, "target": map[string]any{"os": "linux", "arch": runtime.GOARCH}, "toolchain": map[string]any{"go": "go1.27.3", "bun": "1.4.4", "cgo_enabled": false}, "artifacts": artifacts, "actions": actions, "client_view": map[string]any{"version": "1.0.0", "source_revision": revision, "protocol": 4, "transport": "go-request", "actions": actions}, "state_compatibility": map[string]any{"reads": caps, "writes": caps}}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "release-manifest.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	return binary
}
func TestSlotUnitsUseVerifiedLayout(t *testing.T) {
	root := t.TempDir()
	runtime := NewHostRuntime(Defaults())
	runtime.Config.ReleaseRoot = root
	runtime.unitRoot = filepath.Join(root, "units")
	for i, slot := range []string{"blue", "green"} {
		binary := installLayoutFixture(t, root, slot, i == 1)
		if err := os.WriteFile(filepath.Join(root, "systemctl"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", root+":"+os.Getenv("PATH"))
		if err := runtime.configureSlotUnit(context.Background(), slot, false); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(runtime.unitRoot, "agent-vm-native@"+slot+".service.d", "10-release.conf"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "/bin/"+binary+" serve ") {
			t.Fatalf("wrong slot executable: %s", data)
		}
	}
}
