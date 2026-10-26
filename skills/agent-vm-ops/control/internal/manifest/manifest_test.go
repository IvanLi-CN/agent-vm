package manifest

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func operationSpec() map[string]any {
	return map[string]any{
		"request":           map[string]any{"schema": 1, "semantic_revision": 1},
		"response":          map[string]any{"schema": 1, "semantic_revision": 1},
		"client_to_control": map[string]any{"schema_min": 1, "schema_max": 1, "semantic_min": 1, "semantic_max": 1},
		"control_to_client": map[string]any{"schema_min": 1, "schema_max": 1, "semantic_min": 1, "semantic_max": 1},
	}
}

func validManifest(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	actions := make(map[string]any, len(actionNames))
	for _, action := range actionNames {
		actions[action] = operationSpec()
	}
	artifacts := make([]any, 0, len(fixedArtifacts))
	for _, expected := range fixedArtifacts {
		path := filepath.Join(root, filepath.FromSlash(expected.path))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		data := []byte("artifact:" + expected.path)
		if expected.path == "bin/agent-vm-control" {
			data = make([]byte, 20)
			copy(data, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
			machine := uint16(62)
			if runtime.GOARCH == "arm64" {
				machine = 183
			}
			binary.LittleEndian.PutUint16(data[18:], machine)
		}
		if err := os.WriteFile(path, data, os.FileMode(expected.mode)); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, os.FileMode(expected.mode)); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(data)
		artifacts = append(artifacts, map[string]any{"name": expected.name, "path": expected.path, "role": expected.role, "sha256": hex.EncodeToString(digest[:]), "mode": expected.mode})
	}
	releaseID := strings.Repeat("a", 40)
	rootValue := map[string]any{
		"format": 2, "release_id": releaseID, "source_revision": releaseID, "protocol": 4,
		"target":    map[string]any{"os": "linux", "arch": runtime.GOARCH},
		"toolchain": map[string]any{"go": GoToolchain, "cgo_enabled": false},
		"artifacts": artifacts, "actions": actions,
		"client_view":   map[string]any{"release_id": releaseID, "source_revision": releaseID, "protocol": 4, "transport": "go-request", "actions": actions},
		"retired_units": retiredUnits,
	}
	manifestPath := filepath.Join(root, "release-manifest.json")
	data, err := json.Marshal(rootValue)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return manifestPath, filepath.Join(root, "byte-fs.ts")
}

func TestLoadVerifiesCompleteGoBundle(t *testing.T) {
	manifestPath, _ := validManifest(t)
	if err := Load(manifestPath); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMetadataReturnsActionCompatibility(t *testing.T) {
	manifestPath, _ := validManifest(t)
	metadata, err := LoadMetadata(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.ReleaseID != strings.Repeat("a", 40) || metadata.Transport != "go-request" || !metadata.ManifestPresent || !metadata.ArtifactsVerified {
		t.Fatalf("metadata identity = %#v", metadata)
	}
	status, ok := metadata.Actions["status"]
	if !ok || status.Request.Schema != 1 || status.ResponseWindow.SemanticMax != 1 {
		t.Fatalf("status metadata = %#v", status)
	}
}

func TestLoadRejectsChangedArtifactBytes(t *testing.T) {
	manifestPath, artifactPath := validManifest(t)
	if err := os.WriteFile(artifactPath, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Load(manifestPath); err == nil || !strings.Contains(err.Error(), "artifact_checksum_mismatch") {
		t.Fatalf("tampered manifest error = %v", err)
	}
}