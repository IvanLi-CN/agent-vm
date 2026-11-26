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

// An installed host only reaches the release units through `install`, because
// `update` runs the previous maintenance binary. Product and material units are
// merged, so a bundle that carries three units still refreshes the request
// adapter from materials.
func TestPublishUnitSourcesMergesProductAndMaterials(t *testing.T) {
	root := t.TempDir()
	product := filepath.Join(root, "product")
	materials := filepath.Join(root, "materials", "assets")
	for _, dir := range []string{filepath.Join(product, "systemd"), materials + "/systemd"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	slotUnit := "[Service]\nExecStart=/slot\n"
	requestUnit := "[Service]\nNoNewPrivileges=no\n"
	if err := os.WriteFile(filepath.Join(product, "systemd", "agent-vm-native@.service"), []byte(slotUnit), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(materials, "systemd", "agent-vm-request@.service"), []byte(requestUnit), 0o644); err != nil {
		t.Fatal(err)
	}
	merged, err := os.MkdirTemp("", "merged-check-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(merged)
	destination := filepath.Join(merged, "systemd")
	if err := os.MkdirAll(destination, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{filepath.Join(root, "materials", "assets", "systemd"), filepath.Join(product, "systemd")} {
		for _, name := range hostUnitAssets {
			if content, readErr := os.ReadFile(filepath.Join(source, name)); readErr == nil {
				if err := os.WriteFile(filepath.Join(destination, name), content, 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	for name, want := range map[string]string{
		"agent-vm-native@.service":  slotUnit,
		"agent-vm-request@.service": requestUnit,
	} {
		got, err := os.ReadFile(filepath.Join(destination, name))
		if err != nil {
			t.Fatalf("%s missing from merge: %v", name, err)
		}
		if string(got) != want {
			t.Fatalf("%s = %q", name, got)
		}
	}
}
