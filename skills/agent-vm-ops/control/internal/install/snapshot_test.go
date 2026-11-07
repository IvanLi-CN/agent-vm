package install

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotBindsConsumedInputsDespiteOriginalReplacement(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{SchemaVersion: 1, Files: map[string]string{}}
	for name := range targets {
		if len(name) >= 8 && name[:8] == "metrics_" {
			continue
		}
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte("original-"+name), 0600); err != nil {
			t.Fatal(err)
		}
		cfg.Files[name] = path
	}
	input := Inputs{Config: filepath.Join(root, "install.json"), Bundle: filepath.Join(root, "bundle.tar"), Materials: filepath.Join(root, "materials")}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(input.Config, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(input.Bundle, []byte("verified-product"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(input.Materials, 0700); err != nil {
		t.Fatal(err)
	}
	original := []byte("verified executable")
	sum := sha256.Sum256(original)
	script := filepath.Join(input.Materials, "release.py")
	if err := os.WriteFile(script, original, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(script, 0755); err != nil {
		t.Fatal(err)
	}
	revision := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	manifest := MaterialsManifest{SchemaVersion: 1, SourceRevision: revision, Files: []Material{{Path: "release.py", SHA256: hex.EncodeToString(sum[:]), Mode: 0755}}}
	data, _ = json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(input.Materials, "materials-manifest.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	snap, cleanup, err := snapshotInputs(input)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	captured, err := LoadConfig(snap.Config)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := inputDigest(captured)
	if err != nil {
		t.Fatal(err)
	}
	// Replace every original input after capture, including a symlink to an
	// unrelated executable. Every installer consumer must still see the copy.
	for _, path := range cfg.Files {
		if err := os.WriteFile(path, []byte("changed"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(input.Bundle, []byte("different-product"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(script); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/bin/sh", script); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateMaterials(snap.Materials, revision); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(snap.Materials, "release.py"))
	if err != nil || string(got) != string(original) {
		t.Fatalf("changed executable consumed: %v", err)
	}
	got, err = os.ReadFile(snap.Bundle)
	if err != nil || string(got) != "verified-product" {
		t.Fatalf("changed product consumed: %v", err)
	}
	next, err := inputDigest(captured)
	if err != nil || next != digest {
		t.Fatalf("journal no longer binds installed bytes: %v", err)
	}
	// A changed source is not silently accepted on the next installation attempt.
	if _, cleanup, err := snapshotInputs(input); err == nil {
		cleanup()
		t.Fatal("replacement symlink accepted on retry")
	}
}
