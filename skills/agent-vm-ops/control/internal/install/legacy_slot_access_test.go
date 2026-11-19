package install

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

func TestLegacySlotAccessOpensOnlyProtectedDirectories(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "bin")
	program := filepath.Join(dir, "agent-vm-server")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(program, []byte("release artifact"), 0755); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	owner := info.Sys().(*syscall.Stat_t)
	socket := "/run/agent-vm-native/blue/app.sock"
	if err := os.Symlink(socket, filepath.Join(root, "app.sock")); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "unexpected")
	if err := os.Symlink(program, bad); err != nil {
		t.Fatal(err)
	}
	if err := openLegacySlotDirectories(root, socket, owner.Uid, owner.Gid); err == nil {
		t.Fatal("unexpected symlink was accepted")
	}
	assertMode := func(path string, want os.FileMode) {
		t.Helper()
		if got, err := os.Stat(path); err != nil || got.Mode().Perm() != want {
			t.Fatalf("expected %o for %s, got %v (%v)", want, path, got, err)
		}
	}
	assertMode(dir, 0700)
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	if err := openLegacySlotDirectories(root, socket, owner.Uid, owner.Gid); err != nil {
		t.Fatal(err)
	}
	assertMode(dir, 0750)
	assertMode(program, 0755)
}

func TestTransferLegacySlotDirectoriesLeavesReleaseFilesRootOwned(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to exercise directory ownership transfer")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	program := filepath.Join(bin, "agent-vm-server")
	if err := os.Mkdir(bin, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(program, []byte("release artifact"), 0755); err != nil {
		t.Fatal(err)
	}
	const deployUID = 65534
	if err := transferLegacySlotDirectories(root, "/run/agent-vm-native/blue/app.sock", deployUID); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]uint32{root: deployUID, bin: deployUID, program: 0} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Sys().(*syscall.Stat_t).Uid; got != want {
			t.Errorf("%s owner = %d, want %d", path, got, want)
		}
	}
}

func TestLegacySlotAccessAdoptsRootOwnedDirectoryGroup(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to exercise legacy directory group adoption")
	}
	group, err := user.LookupGroup("agent-vm-web")
	if err != nil {
		t.Skip("requires the dedicated slot access group")
	}
	gid, err := strconv.ParseUint(group.Gid, 10, 32)
	if err != nil || gid == 0 {
		t.Fatal("invalid slot access group")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "agent-vm-server"), []byte("release artifact"), 0755); err != nil {
		t.Fatal(err)
	}
	const socket = "/run/agent-vm-native/blue/app.sock"
	if err := os.Symlink(socket, filepath.Join(root, "app.sock")); err != nil {
		t.Fatal(err)
	}
	if err := openLegacySlotDirectories(root, socket, 0, uint32(gid)); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{root, bin} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		stat := info.Sys().(*syscall.Stat_t)
		if stat.Gid != uint32(gid) || info.Mode().Perm() != 0750 {
			t.Fatalf("directory %s = gid %d mode %o, want gid %d mode 750", path, stat.Gid, info.Mode().Perm(), gid)
		}
	}
}
