package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/config"
)

func TestValidateServeSocketOverride(t *testing.T) {
	var cfg config.Config
	if value, err := validateServeSocketOverride("", "", cfg); err != nil || value != "" {
		t.Fatalf("empty socket override = %q, %v", value, err)
	}
	if value, err := validateServeSocketOverride("/run/agent-vm-native/blue/app.sock", "blue", cfg); err != nil || value != "/run/agent-vm-native/blue/app.sock" {
		t.Fatalf("absolute socket override = %q, %v", value, err)
	}
	if _, err := validateServeSocketOverride("run/agent-vm-native/blue/app.sock", "blue", cfg); err == nil {
		t.Fatal("relative socket override was accepted")
	}
	if _, err := validateServeSocketOverride("/run/agent-vm-native/blue/app.sock", "", cfg); err == nil {
		t.Fatal("socket override without a slot identity was accepted")
	}
	cfg.Management.Authority.SocketPath = "/run/agent-vm-native/authority.sock"
	cfg.Management.ForwardAuth.SocketPath = "/run/agent-vm-native/forward-auth.sock"
	for _, reserved := range []string{
		"/run/agent-vm-native/authority.sock",
		"/run/agent-vm-native/forward-auth.sock",
		"/run/agent-vm-native/blue/../green/app.sock",
	} {
		if _, err := validateServeSocketOverride(reserved, "blue", cfg); err == nil {
			t.Fatalf("reserved socket override was accepted: %s", reserved)
		}
	}
	if _, err := validateServeSocketOverride("/run/agent-vm-native/green/app.sock", "blue", cfg); err == nil {
		t.Fatal("cross-slot socket override was accepted")
	}
}

func TestValidateServeInvocationBindsSlotIdentity(t *testing.T) {
	var cfg config.Config
	if _, err := validateServeInvocation("", "blue", "/opt/agent-vm-native/slots/blue/release-manifest.json", cfg); err == nil {
		t.Fatal("slot serve without a socket override was accepted")
	}
	if _, err := validateServeInvocation("/run/agent-vm-native/blue/app.sock", "blue", installedManifestPath, cfg); err == nil {
		t.Fatal("slot serve with the shared manifest path was accepted")
	}
	if _, err := validateServeInvocation("/run/agent-vm-native/blue/app.sock", "test", "/opt/agent-vm-native/slots/test/release-manifest.json", cfg); err == nil {
		t.Fatal("non-blue-green slot identity was accepted")
	}
	if socket, err := validateServeInvocation("/run/agent-vm-native/blue/app.sock", "blue", "/opt/agent-vm-native/slots/blue/release-manifest.json", cfg); err != nil || socket != "/run/agent-vm-native/blue/app.sock" {
		t.Fatalf("valid slot invocation = %q, %v", socket, err)
	}
}

func TestValidateServeSocketParentsRejectsSymlinkAncestorBelowMissingSlot(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	runRoot := filepath.Join(root, "run")
	if err := os.Mkdir(runRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(runRoot, "agent-vm-native")); err != nil {
		t.Fatal(err)
	}
	value := filepath.Join(runRoot, "agent-vm-native", "blue", "app.sock")
	if err := validateServeSocketParents(value); err == nil {
		t.Fatal("socket parent traversal accepted a symlink ancestor behind a missing slot directory")
	}
}

func TestStateBackendDoesNotExposeDSN(t *testing.T) {
	for _, test := range []struct {
		name  string
		value string
		want  string
	}{
		{name: "sqlite", value: "/var/lib/agent-vm-native/state.sqlite3", want: "sqlite"},
		{name: "rqlite with credentials", value: "https://user:secret@example.invalid/db?x=1", want: "rqlite"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := stateBackend(test.value); got != test.want {
				t.Fatalf("stateBackend(%q) = %q, want %q", test.value, got, test.want)
			}
		})
	}
}
