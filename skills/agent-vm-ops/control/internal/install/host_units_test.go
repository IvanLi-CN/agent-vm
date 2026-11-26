package install

import (
	"os"
	"path/filepath"
	"testing"
)

// The slot drop-ins never override ExecStartPre/ExecStartPost, so a stale
// singleton keeps launching the previous maintenance binary; the request unit
// keeps NO_NEW_PRIVS on an adapter that must setuid. Both are release-owned and
// must follow the bundle.
func TestPublishHostUnitsOverwritesStaleReleaseUnits(t *testing.T) {
	product := t.TempDir()
	units := filepath.Join(product, "systemd")
	if err := os.MkdirAll(units, 0o755); err != nil {
		t.Fatal(err)
	}
	next := "[Service]\nExecStart=/new\n"
	for _, name := range hostUnitAssets {
		if err := os.WriteFile(filepath.Join(units, name), []byte(next), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	previous := hostUnitAssets
	defer func() { hostUnitAssets = previous }()
	names := []string{"agent-vm-native@.service"}
	hostUnitAssets = names
	targetRoot := t.TempDir()
	live := filepath.Join(targetRoot, names[0])
	if err := os.WriteFile(live, []byte("[Service]\nExecStart=/old\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if published, err := publishHostUnitsInto(product, targetRoot, false); err != nil {
		t.Fatal(err)
	} else if len(published) != 1 {
		t.Fatalf("published = %v", published)
	}
	installed, err := os.ReadFile(live)
	if err != nil {
		t.Fatal(err)
	}
	if string(installed) != next {
		t.Fatalf("unit was not replaced:\n%s", installed)
	}
	// A second run is a no-op.
	if published, err := publishHostUnitsInto(product, targetRoot, false); err != nil || len(published) != 0 {
		t.Fatalf("second publish changed %v, err %v", published, err)
	}
}
