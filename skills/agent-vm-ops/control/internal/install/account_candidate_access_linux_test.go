//go:build linux

package install

import (
	"bytes"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

func TestCandidateConfigPermissionPreservesContents(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root in an isolated Linux test environment")
	}
	group, err := user.LookupGroup("agent-vm-server")
	if err != nil {
		t.Skip("requires the dedicated server group")
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil || gid <= 0 {
		t.Fatal("invalid fixture group")
	}
	path := filepath.Join(t.TempDir(), "config.json")
	original := []byte("{\"slot\":\"green\"}\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := grantCandidateConfigRead(path, []byte("{\"slot\":\"blue\"}\n"), gid); err == nil {
		t.Fatal("accepted mismatched contents")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("permission changed after rejection: %v, %v", info, err)
	}
	if err := grantCandidateConfigRead(path, original, gid); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(path)
	if err != nil || info.Mode().Perm() != 0640 || int(info.Sys().(*syscall.Stat_t).Gid) != gid {
		t.Fatalf("expected server group to be granted read-only access: %v, %v", info, err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("configuration contents changed: %v", err)
	}
}
