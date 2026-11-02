//go:build linux

package httpserver

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenUnixSocketParentRejectsSymlinkedAncestor(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	parentRoot := filepath.Join(root, "run")
	if err := os.Mkdir(parentRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(parentRoot, "agent-vm-native")); err != nil {
		t.Fatal(err)
	}

	if listener, err := listenUnixSocket(filepath.Join(parentRoot, "agent-vm-native", "blue", "app.sock"), 0o600); err == nil {
		_ = listener.Close()
		t.Fatal("socket parent traversal accepted a symlinked ancestor")
	}
}

func TestListenUnixSocketAnchorsSocketInVerifiedParent(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "nested", "app.sock")
	listener, err := listenUnixSocket(socket, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	info, err := os.Lstat(socket)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket mode = %v", info.Mode())
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("socket permissions = %o", info.Mode().Perm())
	}
	parentInfo, err := os.Stat(filepath.Dir(socket))
	if err != nil {
		t.Fatal(err)
	}
	if parentInfo.Mode().Perm() != 0o750 {
		t.Fatalf("socket parent permissions = %o", parentInfo.Mode().Perm())
	}
}
