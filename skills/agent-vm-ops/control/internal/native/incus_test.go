package native

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

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
	for _, testCase := range []struct {
		name string
		err  error
	}{
		{name: "deadline", err: context.DeadlineExceeded},
		{name: "canceled", err: context.Canceled},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := &CommandError{Result: Result{Stderr: []byte("instance not found")}, Cause: testCase.err}
			if IsNotFound(err) {
				t.Fatalf("IsNotFound accepted %s", testCase.name)
			}
		})
	}
	for _, testCase := range []struct {
		name   string
		result Result
	}{
		{name: "stdout truncated", result: Result{Stdout: []byte("instance not found"), StdoutTruncated: true}},
		{name: "stderr truncated", result: Result{Stderr: []byte("instance not found"), StderrTruncated: true}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if IsNotFound(&CommandError{Result: testCase.result, Cause: errors.New("incus command failed")}) {
				t.Fatalf("IsNotFound accepted truncated diagnostic")
			}
		})
	}
}

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
func TestIncusImageQueryUsesProjectAndParsesIdentity(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "incus-argv")
	fingerprint := strings.Repeat("d", 64)
	script := `#!/bin/sh
printf '%s\n' "$@" > "$INCUS_ARGV_LOG"
printf '{"fingerprint":"%s","architecture":"x86_64","type":"container","properties":{"os":"debian"}}\n' "$INCUS_IMAGE_FINGERPRINT"
`
	if err := os.WriteFile(filepath.Join(binDir, "incus"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("INCUS_ARGV_LOG", logPath)
	t.Setenv("INCUS_IMAGE_FINGERPRINT", fingerprint)

	client := NewIncusClient(NewRunner(), "agents")
	info, err := client.QueryImage(context.Background(), fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if info.Fingerprint != fingerprint || info.Architecture != "x86_64" || info.InstanceType != "container" || info.Properties["os"] != "debian" {
		t.Fatalf("image info = %#v", info)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{"query", "/1.0/images/" + fingerprint + "?project=agents"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("incus argv = %#v, want %#v", got, want)
	}
}

func TestIncusRemoteImageSizeUsesConfiguredSourceAndFingerprint(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "incus-argv")
	fingerprint := strings.Repeat("e", 64)
	script := `#!/bin/sh
printf '%s\n' "$@" > "$INCUS_ARGV_LOG"
printf '{"fingerprint":"%s","architecture":"x86_64","type":"container","size":4096}\n' "$INCUS_IMAGE_FINGERPRINT"
`
	if err := os.WriteFile(filepath.Join(binDir, "incus"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("INCUS_ARGV_LOG", logPath)
	t.Setenv("INCUS_IMAGE_FINGERPRINT", fingerprint)

	client := NewIncusClient(NewRunner(), "agents")
	size, err := client.QueryRemoteImageSize(context.Background(), "approved", fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if size != 4096 {
		t.Fatalf("remote image size = %d, want 4096", size)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{"--project", "agents", "image", "info", "approved:" + fingerprint, "--format", "json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("incus argv = %#v, want %#v", got, want)
	}
}

func TestIncusStoragePoolAvailableBytesUsesPoolResources(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "incus-argv")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$INCUS_ARGV_LOG\"\nprintf '%s\\n' '{\"space\":{\"space_available\":8192}}'\n"
	if err := os.WriteFile(filepath.Join(binDir, "incus"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("INCUS_ARGV_LOG", logPath)
	client := NewIncusClient(NewRunner(), "agents", ClientOptions{Pool: "managed-pool"})
	available, err := client.StoragePoolAvailableBytes(context.Background())
	if err != nil || available != 8192 {
		t.Fatalf("available=%d err=%v", available, err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{"query", "/1.0/storage-pools/managed-pool/resources?project=agents"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("incus argv = %#v, want %#v", got, want)
	}
}

func TestIncusVerificationInitDisablesProfilesAndNetwork(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "incus-argv")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$INCUS_ARGV_LOG\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "incus"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("INCUS_ARGV_LOG", logPath)

	client := NewIncusClient(NewRunner(), "agents", ClientOptions{Profile: "production", Network: "agentsbr0", Pool: "agents"})
	if err := client.InitVerification(context.Background(), "image-fingerprint", "image-check"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{"--project", "agents", "init", "image-fingerprint", "image-check", "--no-profiles", "--storage", "agents", "--config", "boot.autostart=false"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("incus argv = %#v, want %#v", got, want)
	}
}

func TestIncusImageCopyUsesRemoteSourceAndLocalTarget(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "incus-argv")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$INCUS_ARGV_LOG\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "incus"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("INCUS_ARGV_LOG", logPath)

	fingerprint := strings.Repeat("a", 64)
	client := NewIncusClient(NewRunner(), "agents")
	if err := client.CopyImage(context.Background(), "approved", fingerprint); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{"--project", "agents", "image", "copy", "approved:" + fingerprint, "local:"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("incus argv = %#v, want %#v", got, want)
	}
}

func TestIncusImageImportUsesPrivateStagingAndParsesFingerprint(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "incus-argv")
	fingerprint := strings.Repeat("b", 64)
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$INCUS_ARGV_LOG\"\nprintf 'Image imported with fingerprint: %s\\n' \"$INCUS_IMAGE_FINGERPRINT\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "incus"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("INCUS_ARGV_LOG", logPath)
	t.Setenv("INCUS_IMAGE_FINGERPRINT", fingerprint)

	stagingPath := filepath.Join(t.TempDir(), "staging")
	if err := os.Mkdir(stagingPath, 0o700); err != nil {
		t.Fatal(err)
	}
	client := NewIncusClient(NewRunner(), "agents")
	gotFingerprint, err := client.ImportImage(context.Background(), stagingPath, "unified", []byte("metadata"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if gotFingerprint != fingerprint {
		t.Fatalf("fingerprint = %q, want %q", gotFingerprint, fingerprint)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(got) != 6 || !reflect.DeepEqual(got[:4], []string{"--project", "agents", "image", "import"}) || got[5] != "local:" {
		t.Fatalf("incus argv = %#v", got)
	}
	stagedPath := got[4]
	relative, err := filepath.Rel(stagingPath, stagedPath)
	if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.Base(stagedPath) != "image" {
		t.Fatalf("staged path = %q, relative = %q", stagedPath, relative)
	}
	entries, err := os.ReadDir(stagingPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("staging entries remain after import: %#v", entries)
	}
}

func TestIncusImageDeleteTreatsMissingImageAsIdempotent(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "incus-argv")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$INCUS_ARGV_LOG\"\nprintf '%s\\n' 'Error: Image not found' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "incus"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("INCUS_ARGV_LOG", logPath)

	fingerprint := strings.Repeat("c", 64)
	client := NewIncusClient(NewRunner(), "agents")
	if err := client.DeleteImage(context.Background(), fingerprint); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{"--project", "agents", "image", "delete", "local:" + fingerprint}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("incus argv = %#v, want %#v", got, want)
	}
}

func TestImageVerificationScriptReportsMissingToolsAsUnsuitable(t *testing.T) {
	command := exec.Command("sh", "-c", verificationScript, "agent-vm-image-check", "bun", "tool-that-is-not-installed")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("verification script error = %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	want := []string{"runtime=true", "sync_tool=false", "guest_ready=false"}
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("verification output = %#v, want %#v", lines, want)
	}
}
