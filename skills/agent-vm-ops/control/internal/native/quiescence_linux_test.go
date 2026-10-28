//go:build linux

package native

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRecoveryCgroupRequiresOnlyAuthorityProcess(t *testing.T) {
	for _, mode := range []string{"only-self", "other-process", "no-process", "child-group", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			value := "42\n"
			if mode == "other-process" {
				value += "43\n"
			}
			if mode == "no-process" {
				value = ""
			}
			if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(value), 0o600); err != nil {
				t.Fatal(err)
			}
			if mode == "child-group" {
				if err := os.Mkdir(filepath.Join(dir, "child"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "symlink" {
				if err := os.Symlink("cgroup.procs", filepath.Join(dir, "link")); err != nil {
					t.Fatal(err)
				}
			}
			err := checkRecoveryCgroup(dir, 42)
			if (err == nil) != (mode == "only-self") {
				t.Fatalf("checkRecoveryCgroup: %v", err)
			}
		})
	}
}
