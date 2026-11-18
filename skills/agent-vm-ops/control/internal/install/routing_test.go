package install

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManagedRoutingPreservesLegacySelection(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	slots := filepath.Join(root, "slots")
	blue := filepath.Join(slots, "blue")
	green := filepath.Join(slots, "green")
	managed := filepath.Join(root, "routing", "active")
	public := filepath.Join(root, "active")
	for _, dir := range []string{blue, green, filepath.Dir(managed)} {
		if err := os.MkdirAll(dir, 0750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("slots/blue", public); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(green, managed); err != nil {
		t.Fatal(err)
	}
	if err := adoptActiveRoute(public, managed, slots); err == nil {
		t.Fatal("accepted conflicting routing paths")
	}
	if got, err := filepath.EvalSymlinks(public); err != nil || got != blue {
		t.Fatalf("conflict changed public selection: %q, %v", got, err)
	}
	if err := os.Remove(managed); err != nil {
		t.Fatal(err)
	}
	for count := 0; count < 2; count++ {
		if err := adoptActiveRoute(public, managed, slots); err != nil {
			t.Fatal(err)
		}
		if got, err := os.Readlink(public); err != nil || got != managed {
			t.Fatalf("public alias = %q, %v", got, err)
		}
		if got, err := filepath.EvalSymlinks(public); err != nil || got != blue {
			t.Fatalf("adoption changed public selection: %q, %v", got, err)
		}
	}
}
