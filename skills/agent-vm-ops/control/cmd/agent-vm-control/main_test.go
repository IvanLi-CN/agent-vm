package main

import "testing"

func TestValidateServeSocketOverride(t *testing.T) {
	if value, err := validateServeSocketOverride(""); err != nil || value != "" {
		t.Fatalf("empty socket override = %q, %v", value, err)
	}
	if value, err := validateServeSocketOverride("/run/agent-vm-native/blue/app.sock"); err != nil || value != "/run/agent-vm-native/blue/app.sock" {
		t.Fatalf("absolute socket override = %q, %v", value, err)
	}
	if _, err := validateServeSocketOverride("run/agent-vm-native/blue/app.sock"); err == nil {
		t.Fatal("relative socket override was accepted")
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
