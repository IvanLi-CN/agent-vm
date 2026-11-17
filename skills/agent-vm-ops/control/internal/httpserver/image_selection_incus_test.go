package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/config"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/lifecycle"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/native"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/protocol"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

// This opt-in acceptance creates containers only in the validation guest's
// disposable nested Incus project. No deployment or production config is read.
func TestClientImageSelectionIsolatedIncus(t *testing.T) {
	path := os.Getenv("AGENT_VM_IMAGE_ACCEPTANCE_CONFIG")
	if path == "" {
		t.Skip("requires session-owned nested Incus acceptance config")
	}
	cfg := config.Defaults()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Project != "client-image-test" && !strings.HasPrefix(cfg.Project, "client-image-test-") {
		t.Fatal("refusing a non-test Incus project")
	}
	fingerprints := strings.Split(os.Getenv("AGENT_VM_IMAGE_ACCEPTANCE_FINGERPRINTS"), ",")
	if len(fingerprints) != 2 || fingerprints[0] == fingerprints[1] {
		t.Fatal("two distinct fingerprints required")
	}
	root := t.TempDir()
	cfg.StateDB = filepath.Join(root, "state.db")
	cfg.StoragePath = filepath.Join(root, "storage")
	cfg.TimerDir = filepath.Join(root, "timers")
	cfg.ReleaseFencePath = filepath.Join(root, "release.fence")
	cfg.MetricsDir = filepath.Join(root, "metrics")
	if err := os.MkdirAll(cfg.StoragePath, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := state.OpenForTest(cfg.StateDB)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	for index, fp := range fingerprints {
		name := fmt.Sprintf("selection-%d", index)
		if err := store.InsertManagedImage(ctx, state.ManagedImage{Fingerprint: fp, Name: &name, Architecture: "x86_64", InstanceType: "container", SourceKind: "local_upload", State: "available", Integrity: "passed", Suitability: "passed", IsDefault: index == 0}); err != nil {
			t.Fatal(err)
		}
	}
	guest := native.NewIncusClient(native.NewRunner(), cfg.Project, native.ClientOptions{Profile: cfg.Profile, Pool: cfg.Pool, Network: cfg.Network, StoragePath: cfg.StoragePath})
	actions := map[string]protocol.ActionCompatibility{}
	for action := range protocol.Actions {
		maximum := int64(1)
		if action == "acquire" {
			maximum = 2
		}
		actions[action] = protocol.ActionCompatibility{Request: protocol.Schema{Schema: 1, SemanticRevision: 1}, Response: protocol.Schema{Schema: 1, SemanticRevision: 1}, RequestWindow: protocol.CompatibilityWindow{SchemaMin: 1, SchemaMax: 1, SemanticMin: 1, SemanticMax: maximum}, ResponseWindow: protocol.CompatibilityWindow{SchemaMin: 1, SchemaMax: 1, SemanticMin: 1, SemanticMax: 1}}
	}
	revision := os.Getenv("AGENT_VM_IMAGE_ACCEPTANCE_SHA")
	if len(revision) != 40 {
		t.Fatal("candidate SHA required")
	}
	release := &protocol.ReleaseMetadata{ReleaseID: revision, SourceRevision: revision, Version: "dev", Protocol: protocol.Version, Actions: actions}
	service := lifecycle.New(store, cfg, lifecycle.Options{Guest: guest, Release: release, LockDir: filepath.Join(root, "locks")})
	service.MarkReady()
	// Cleanup is scoped to tasks created by this store, including partial failures.
	defer func() {
		tasks, _ := store.ListTasks(context.Background())
		for _, task := range tasks {
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_ = guest.Delete(cleanup, task.InstanceName)
			cancel()
		}
	}()
	socket := filepath.Join(root, "s")
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- Serve(ctx, socket, service) }()
	for deadline := time.Now().Add(5 * time.Second); ; {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("test socket unavailable")
		}
		select {
		case err := <-serverErrors:
			t.Fatal(err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	clientRoot := os.Getenv("AGENT_VM_IMAGE_ACCEPTANCE_CLIENT")
	binary := os.Getenv("AGENT_VM_IMAGE_ACCEPTANCE_CONTROL")
	if clientRoot == "" || binary == "" {
		t.Fatal("portable client and control adapter paths required")
	}
	if _, err := os.Stat(filepath.Join(clientRoot, "client-release.json")); !os.IsNotExist(err) {
		t.Fatal("client distribution must have no sidecar")
	}
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	// SSH is a local transport shim into the real fixed UDS request adapter;
	// lifecycle effects, Incus, the service and the public Bun client are real.
	shim := "#!/bin/sh\nfor arg do last=\"$arg\"; done\nexec /bin/sh -c \"$last\"\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(shim), 0700); err != nil {
		t.Fatal(err)
	}
	configRoot := filepath.Join(root, "config")
	if err := os.MkdirAll(filepath.Join(configRoot, "agent-vm"), 0700); err != nil {
		t.Fatal(err)
	}
	host := fmt.Sprintf("default_host = \"isolated\"\n[hosts.isolated]\ntransport = \"ssh\"\ncontrol_host = \"local-test\"\nremote_request = %q\nremote_socket = %q\n", binary, socket)
	if err := os.WriteFile(filepath.Join(configRoot, "agent-vm/config.toml"), []byte(host), 0600); err != nil {
		t.Fatal(err)
	}
	call := func(args ...string) (map[string]any, error) {
		cmd := exec.CommandContext(ctx, "bun", append([]string{filepath.Join(clientRoot, "agent_vm.ts")}, args...)...)
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "XDG_CONFIG_HOME="+configRoot, "XDG_STATE_HOME="+filepath.Join(root, "client-state"), "AGENT_VM_PROJECT_REMOTE=", "AGENT_VM_SESSION_ID=")
		output, err := cmd.CombinedOutput()
		var result map[string]any
		if decodeErr := json.Unmarshal(output, &result); decodeErr != nil {
			return nil, fmt.Errorf("CLI %v: %v %s", args, err, output)
		}
		return result, err
	}
	acquire := func(session, op, selector string) map[string]any {
		t.Helper()
		result, err := call("acquire", "--session-id", session, "--operation-id", op, "--image", selector, "--cpu", "1", "--memory", "512MiB", "--disk", "4GiB")
		if err != nil {
			t.Fatalf("acquire %s: %v %#v", session, err, result)
		}
		return result["result"].(map[string]any)
	}
	list, err := call("image", "list")
	if err != nil {
		t.Fatalf("list: %v %#v", err, list)
	}
	first := acquire("accept-one", "accept-create-one", "selection-0")
	second := acquire("accept-two", "accept-create-two", fingerprints[1])
	if first["image_fingerprint"] != fingerprints[0] || second["image_fingerprint"] != fingerprints[1] {
		t.Fatalf("wrong images: %#v %#v", first, second)
	}
	assertMarker := func(result map[string]any, want string) {
		t.Helper()
		out, err := guest.Exec(ctx, result["instance_name"].(string), []string{"cat", "/etc/agent-vm-test-image"}, nil)
		if err != nil || strings.TrimSpace(string(out.Stdout)) != want {
			t.Fatalf("guest marker: %v %q", err, out.Stdout)
		}
	}
	assertMarker(first, "A")
	assertMarker(second, "B")
	pendingRequest := protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": "accept-pending", "operation_id": "accept-create-pending", "generation": int64(0), "image": "selection-0", "compatibility": map[string]any{"release_id": "legacy-protocol-4", "protocol": protocol.Version, "action": "acquire", "request": map[string]any{"schema": 1, "semantic_revision": 2}, "accepted_response": map[string]any{"schema_min": 1, "schema_max": 1, "semantic_min": 1, "semantic_max": 1}}}
	digest, _ := protocol.Digest(pendingRequest)
	session := "accept-pending"
	generation := int64(0)
	if _, err := store.ClaimOperation(ctx, "accept-create-pending", "acquire", &session, &generation, map[string]any{"outcome": "pending", "_request_digest": digest, "_request": map[string]any(pendingRequest)}, state.AcquireImageSelection{}); err != nil {
		t.Fatal(err)
	}
	version, _ := store.ImageCatalogVersion(ctx)
	if _, err := store.SetImageName(ctx, fingerprints[1], nil, version, "accept-clear", "admin", "acceptance"); err != nil {
		t.Fatal(err)
	}
	version, _ = store.ImageCatalogVersion(ctx)
	name := "selection-0"
	if _, err := store.SetImageName(ctx, fingerprints[1], &name, version, "accept-rebind", "admin", "acceptance"); err != nil {
		t.Fatal(err)
	}
	restarted := lifecycle.New(store, cfg, lifecycle.Options{Guest: guest, Release: release})
	restarted.MarkReady()
	if _, err := restarted.Dispatch(ctx, pendingRequest); err == nil {
		t.Fatal("pending operation replayed native creation")
	}
	pinned, err := store.GetOperation(ctx, "accept-create-pending")
	if err != nil || pinned.Response["_image_fingerprint"] != fingerprints[0] {
		t.Fatal("recovery reselected image")
	}
	if task, _ := store.GetTask(ctx, map[string]any{"session_id": "accept-pending"}); task != nil {
		t.Fatal("uncertain recovery created a new task")
	}
	replay := acquire("accept-one", "accept-create-one", name)
	if replay["image_fingerprint"] != fingerprints[0] {
		t.Fatal("completed operation reselected image")
	}
	third := acquire("accept-three", "accept-create-three", name)
	if third["image_fingerprint"] != fingerprints[1] {
		t.Fatal("new session ignored rebind")
	}
	assertMarker(third, "B")
	before, _ := store.GetTask(ctx, map[string]any{"session_id": "accept-one"})
	beforeJSON, _ := json.Marshal(before)
	conflict, err := call("acquire", "--session-id", "accept-one", "--operation-id", "accept-conflict", "--image", name)
	if err == nil || conflict["error"].(map[string]any)["code"] != "image_conflict" {
		t.Fatalf("conflict: %#v %v", conflict, err)
	}
	after, _ := store.GetTask(ctx, map[string]any{"session_id": "accept-one"})
	afterJSON, _ := json.Marshal(after)
	if string(beforeJSON) != string(afterJSON) {
		t.Fatal("image conflict changed original task")
	}
	assertMarker(first, "A")
	t.Logf("candidate=%s two_images=true name_and_fingerprint=true rebind_new_only=true replay_pinned=true pending_recovery_pinned=true conflict_preserved=true", revision)
}
