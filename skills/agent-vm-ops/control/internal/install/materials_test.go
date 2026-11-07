package install

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestMaterialsRejectRevisionTamperAndLinks(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("example")
	digest := sha256.Sum256(content)
	path := filepath.Join(root, "example.json")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
	revision := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	manifest := MaterialsManifest{SchemaVersion: 1, SourceRevision: revision, Files: []Material{{Path: "example.json", SHA256: hex.EncodeToString(digest[:]), Mode: 0644}}}
	data, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(root, "materials-manifest.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateMaterials(root, revision); err != nil {
		t.Fatal(err)
	}
	extra := filepath.Join(root, "unlisted-script")
	if err := os.WriteFile(extra, []byte("unverified"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateMaterials(root, revision); err == nil {
		t.Fatal("unlisted material accepted")
	}
	if err := os.Remove(extra); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateMaterials(root, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"); err == nil {
		t.Fatal("revision mismatch accepted")
	}
	if err := os.WriteFile(path, []byte("tampered"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateMaterials(root, revision); err == nil {
		t.Fatal("tamper accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", path); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateMaterials(root, revision); err == nil {
		t.Fatal("link accepted")
	}
}
