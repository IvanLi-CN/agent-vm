package install

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLegacySlotLockMountRejectsUnsafePaths(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"relative/pool",
		"/etc/../var",
		"/var/lib/pool\nUser=root",
		"/var/lib/%n",
		"/var/lib/source:/etc",
		link,
	} {
		t.Run(path, func(t *testing.T) {
			if unit, err := legacySlotLockMount(path); err == nil {
				t.Fatalf("unsafe mount path produced a systemd directive: %q", unit)
			}
		})
	}
}
