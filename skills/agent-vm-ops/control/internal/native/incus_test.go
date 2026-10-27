package native

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestIncusInitUsesSystemContainerArguments(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "incus-argv")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$INCUS_ARGV_LOG\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "incus"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("INCUS_ARGV_LOG", logPath)

	client := NewIncusClient(NewRunner(), "agents", ClientOptions{
		Profile: "agent-vm",
		Network: "agentsbr0",
		Pool:    "agents",
	})
	if err := client.Init(context.Background(), "images:debian/13", "container-check"); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{
		"--project", "agents", "init", "images:debian/13", "container-check",
		"--profile", "agent-vm", "--network", "agentsbr0", "--storage", "agents",
		"--config", "boot.autostart=false",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("incus argv = %#v, want %#v", got, want)
	}
	for _, arg := range got {
		if arg == "--vm" {
			t.Fatal("container initialization must not request an Incus VM")
		}
	}
}

func TestIncusQueryUsesProjectQueryParameter(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "incus-argv")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$INCUS_ARGV_LOG\"\nprintf '%s\\n' '{\"config\":{\"user.agent-vm.managed\":\"native-v1\"}}'\n"
	if err := os.WriteFile(filepath.Join(binDir, "incus"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("INCUS_ARGV_LOG", logPath)

	client := NewIncusClient(NewRunner(), "agents")
	value, err := client.Config(context.Background(), "instance-check")
	if err != nil {
		t.Fatal(err)
	}
	if value["config"].(map[string]any)["user.agent-vm.managed"] != "native-v1" {
		t.Fatalf("query response = %#v", value)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{"query", "/1.0/instances/instance-check?project=agents"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("incus argv = %#v, want %#v", got, want)
	}
}

func TestIsNotFoundRequiresInstanceSpecificDiagnostic(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
		want   bool
	}{
		{name: "instance missing", stderr: "Error: Instance not found", want: true},
		{name: "container missing", stderr: "Error: container alpha does not exist", want: true},
		{name: "no such instance", stderr: "Error: no such instance", want: true},
		{name: "project missing", stderr: "Error: project not found", want: false},
		{name: "project diagnostic mentions instance", stderr: "Error: query instance: project not found", want: false},
		{name: "remote missing", stderr: "Error: remote endpoint does not exist", want: false},
		{name: "generic 404", stderr: "HTTP 404 Not Found", want: false},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			err := &CommandError{Result: Result{Stderr: []byte(testCase.stderr)}, Cause: errors.New("incus command failed")}
			if got := IsNotFound(err); got != testCase.want {
				t.Fatalf("IsNotFound(%q) = %v, want %v", testCase.stderr, got, testCase.want)
			}
		})
	}
}
