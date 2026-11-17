package main

import "testing"

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
