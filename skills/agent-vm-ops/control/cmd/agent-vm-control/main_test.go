package main

import (
	"testing"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/config"
)

func TestValidateServeSocketOverride(t *testing.T) {
	var cfg config.Config
	if value, err := validateServeSocketOverride("", cfg); err != nil || value != "" {
		t.Fatalf("empty socket override = %q, %v", value, err)
	}
	if value, err := validateServeSocketOverride("/run/agent-vm-native/blue/../blue/app.sock", cfg); err != nil || value != "/run/agent-vm-native/blue/app.sock" {
		t.Fatalf("absolute socket override = %q, %v", value, err)
	}
	if _, err := validateServeSocketOverride("run/agent-vm-native/blue/app.sock", cfg); err == nil {
		t.Fatal("relative socket override was accepted")
	}
	cfg.Management.Authority.SocketPath = "/run/agent-vm-native/authority.sock"
	cfg.Management.ForwardAuth.SocketPath = "/run/agent-vm-native/forward-auth.sock"
	for _, reserved := range []string{
		"/run/agent-vm-native/authority.sock",
		"/run/agent-vm-native/forward-auth.sock",
		"/run/agent-vm-native/forward-auth/../forward-auth.sock",
	} {
		if _, err := validateServeSocketOverride(reserved, cfg); err == nil {
			t.Fatalf("reserved socket override was accepted: %s", reserved)
		}
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
