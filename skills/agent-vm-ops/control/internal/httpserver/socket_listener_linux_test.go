//go:build linux

package httpserver

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
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

func TestInstallUnixSocketRestoresPreviousAfterValidationFailure(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "app.sock")
	previous, err := listenUnixSocket(socket, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := previous.Close(); err != nil {
		t.Fatal(err)
	}

	parentFD, name, err := openUnixSocketParent(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(parentFD)
	var before unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		t.Fatal(err)
	}

	socketFD, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(socketFD)
	temporaryName, err := bindTemporaryUnixSocket(parentFD, socketFD, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Unlinkat(parentFD, temporaryName, 0)
	if err := unix.Listen(socketFD, unixSocketBacklog); err != nil {
		t.Fatal(err)
	}
	if err := chmodUnixSocketAt(parentFD, temporaryName, 0o600); err != nil {
		t.Fatal(err)
	}
	identityFD, err := unix.Dup(socketFD)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(identityFD)

	cleanup, err := installUnixSocketAt(parentFD, temporaryName, name, identityFD, 0o660)
	if err == nil {
		t.Fatal("expected the deliberate mode mismatch to fail")
	}
	if !cleanup {
		t.Fatal("failed replacement left the temporary socket in an uncertain location")
	}

	var after unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &after, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		t.Fatal(err)
	}
	if after.Dev != before.Dev || after.Ino != before.Ino {
		t.Fatalf("previous socket identity changed: before=%d:%d after=%d:%d", before.Dev, before.Ino, after.Dev, after.Ino)
	}
}
