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
	if err := client.Init(context.Background(), "images:debian/13", "container-check", map[string]string{"user.agent-vm.managed": "native-v1", "user.agent-vm.task": "task", "user.agent-vm.session": "session", "user.agent-vm.generation": "1"}); err != nil {
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
		"--config", "user.agent-vm.generation=1", "--config", "user.agent-vm.managed=native-v1",
		"--config", "user.agent-vm.session=session", "--config", "user.agent-vm.task=task",
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

func TestIsNotFoundRejectsUnrelatedOrUncertainFailures(t *testing.T) {
	for _, diagnostic := range []string{"project not found", "server returned 404", "transport error: route does not exist"} {
		err := &CommandError{Result: Result{Stderr: []byte(diagnostic)}, Cause: errors.New("command failed")}
		if IsNotFound(err) {
			t.Errorf("unrelated failure %q classified as missing instance", diagnostic)
		}
	}
	err := &CommandError{Result: Result{Stderr: []byte("instance not found")}, Cause: context.DeadlineExceeded}
	if IsNotFound(err) {
		t.Fatal("timed-out query classified as missing instance")
	}
}

func TestIncusConfigClassifiesConfirmedMissingInstance(t *testing.T) {
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "incus"), []byte("#!/bin/sh\necho 'Error: Instance not found' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err := NewIncusClient(NewRunner(), "agents").Config(context.Background(), "missing")
	if !IsNotFound(err) {
		t.Fatalf("config error = %v, want confirmed instance missing", err)
	}
}
