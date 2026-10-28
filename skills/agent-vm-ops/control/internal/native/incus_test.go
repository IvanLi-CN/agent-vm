package native

import (
	"context"
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
