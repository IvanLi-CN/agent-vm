package install

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestRecoveryOwnershipHandoffPreservesMaterial(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root in an isolated Linux environment")
	}
	parent, err := os.MkdirTemp("/run", "agent-vm-recovery-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	root := filepath.Join(parent, "migration")
	point := filepath.Join(root, "rqlite-recovery", "point-example")
	if err := os.MkdirAll(point, 0700); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(point, "snapshot.sqlite3")
	if err := os.WriteFile(snapshot, []byte("existing snapshot"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	const uid, gid = 65534, 65534
	assertOwner := func(path string, want uint32) {
		t.Helper()
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Sys().(*syscall.Stat_t).Uid != want {
			t.Fatalf("unexpected owner of %s", path)
		}
	}
	for _, link := range []func(string, string) error{os.Symlink, os.Link} {
		bad := filepath.Join(root, "unexpected")
		if err := link(snapshot, bad); err != nil {
			t.Fatal(err)
		}
		if err := transferRecoveryOwnership(t.Context(), root, uid, gid); err == nil {
			t.Fatal("accepted a linked recovery file")
		}
		assertOwner(root, 0)
		assertOwner(snapshot, 0)
		if err := os.Remove(bad); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := transferRecoveryOwnership(t.Context(), root, uid, gid); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{root, filepath.Dir(point), point, snapshot} {
		assertOwner(path, uid)
	}
	after, err := os.Stat(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(snapshot)
	if err != nil || string(content) != "existing snapshot" || !os.SameFile(before, after) ||
		!before.ModTime().Equal(after.ModTime()) || after.Mode() != before.Mode() {
		t.Fatalf("existing recovery material changed: %v", err)
	}
}
