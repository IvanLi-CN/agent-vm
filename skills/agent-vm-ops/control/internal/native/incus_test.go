package native

import (
	"context"
	"os"
	"os/exec"
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

func TestIncusStoragePoolCapacityVerifiesDirectoryPoolSource(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "incus-argv")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$INCUS_ARGV_LOG"
printf '%s\n' '{"name":"agents","driver":"dir","config":{"source":"'"$INCUS_STORAGE_PATH"'"}}'
`
	if err := os.WriteFile(filepath.Join(binDir, "incus"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	storagePath := t.TempDir()
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("INCUS_ARGV_LOG", logPath)
	t.Setenv("INCUS_STORAGE_PATH", storagePath)

	client := NewIncusClient(NewRunner(), "agents", ClientOptions{Pool: "agents", StoragePath: storagePath})
	capacity, err := client.StoragePoolCapacity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if capacity.TotalBytes <= 0 || capacity.FreeBytes <= 0 || capacity.FreeBytes > capacity.TotalBytes {
		t.Fatalf("capacity = %#v", capacity)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{"query", "/1.0/storage-pools/agents?project=default"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("incus argv = %#v, want %#v", got, want)
	}
}

func TestStorageCapacityRejectsBlockMultiplicationOverflow(t *testing.T) {
	if _, err := storageCapacityFromBlocks(^uint64(0), 1, 2); err == nil {
		t.Fatal("overflowing total capacity was accepted")
	}
	if _, err := storageCapacityFromBlocks(2, ^uint64(0), 2); err == nil {
		t.Fatal("overflowing free capacity was accepted")
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

func TestIsNotFoundRejectsUnrelatedOrUncertainFailures(t *testing.T) {
	exitErr := exec.Command("false").Run()
	for _, diagnostic := range []string{"project not found", "server returned 404", "transport error: route does not exist"} {
		err := &CommandError{Result: Result{Code: 1, Stderr: []byte(diagnostic)}, Cause: exitErr}
		if IsNotFound(err, "missing", "agents") {
			t.Errorf("unrelated failure %q classified as missing instance", diagnostic)
		}
	}
	err := &CommandError{Result: Result{Stderr: []byte("instance not found")}, Cause: context.DeadlineExceeded}
	if IsNotFound(err, "missing", "agents") {
		t.Fatal("timed-out query classified as missing instance")
	}
	for _, result := range []Result{
		{Code: 1, Stderr: []byte("instance not found"), StdoutTruncated: true},
		{Code: 1, Stderr: []byte("instance not found"), StderrTruncated: true},
	} {
		if IsNotFound(&CommandError{Result: result, Cause: exitErr}, "missing", "agents") {
			t.Fatal("truncated diagnostic classified as missing instance")
		}
	}
}

func TestIncusConfigClassifiesConfirmedMissingInstance(t *testing.T) {
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "incus"), []byte("#!/bin/sh\necho 'Error: Instance not found' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err := NewIncusClient(NewRunner(), "agents").Config(context.Background(), "missing")
	if !IsNotFound(err, "missing", "agents") {
		t.Fatalf("config error = %v, want confirmed instance missing", err)
	}
}

func TestIncusConfigRecognizesContextualMissingInstance(t *testing.T) {
	binDir := t.TempDir()
	script := "#!/bin/sh\necho 'Error: Failed to fetch instance \"missing\" in projectName \"agents\": Instance not found' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "incus"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err := NewIncusClient(NewRunner(), "agents").Config(context.Background(), "missing")
	if !IsNotFound(err, "missing", "agents") {
		t.Fatalf("contextual not-found not recognized: %v", err)
	}
	if IsNotFound(err, "other", "agents") || IsNotFound(err, "missing", "other") {
		t.Fatal("unrelated instance or project classified as absent")
	}
}
