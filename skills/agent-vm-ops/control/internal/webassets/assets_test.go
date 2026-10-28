package webassets

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"testing/fstest"
)

func TestVerifyFSRejectsTamperedAsset(t *testing.T) {
	digest := sha256.Sum256([]byte("index"))
	entries := []Entry{{Path: "index.html", SHA256: hex.EncodeToString(digest[:]), Size: 5}}
	files := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("index")}}
	if err := VerifyFS(files, entries); err != nil {
		t.Fatal(err)
	}
	files["index.html"] = &fstest.MapFile{Data: []byte("altered")}
	if err := VerifyFS(files, entries); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("tampered asset error = %v", err)
	}
}

func TestVerifyFSRequiresIndex(t *testing.T) {
	digest := sha256.Sum256([]byte("asset"))
	if err := VerifyFS(fstest.MapFS{"app.js": &fstest.MapFile{Data: []byte("asset")}}, []Entry{{Path: "app.js", SHA256: hex.EncodeToString(digest[:]), Size: 5}}); err == nil || !strings.Contains(err.Error(), "index.html") {
		t.Fatalf("missing index error = %v", err)
	}
}
