package install

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/config"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/manifest"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/native"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

const recordPath = "/var/lib/agent-vm-native/deployment/install.json"
const cachedBundle = "/var/lib/agent-vm-native/deployment/install-bundle.tar"

type record struct {
	SchemaVersion  int    `json:"schema_version"`
	SourceRevision string `json:"source_revision"`
	BundleSHA256   string `json:"bundle_sha256"`
	InputsSHA256   string `json:"inputs_sha256"`
	TransactionID  string `json:"transaction_id"`
	Phase          string `json:"phase"`
}

func inputDigest(cfg Config) (string, error) {
	names := make([]string, 0, len(cfg.Files))
	for name := range cfg.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, name := range names {
		data, err := protectedRead(cfg.Files[name])
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(data)
		fmt.Fprintf(h, "%s:%x\n", name, sum)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func saveRecord(r record) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := safeParents(filepath.Dir(recordPath)); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(recordPath), ".record-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), recordPath); err != nil {
		return err
	}
	return syncDir(filepath.Dir(recordPath))
}
func command(ctx context.Context, name string, args ...string) error {
	if err := exec.CommandContext(ctx, name, args...).Run(); err != nil {
		return fmt.Errorf("installation command %s failed: %w", name, err)
	}
	return nil
}
func waitDaemon(ctx context.Context, client deployd.Client, timeout time.Duration) error {
	readyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, _, err := client.Call(readyCtx, "GET", "/v1/status", nil); err == nil {
			return nil
		}
		select {
		case <-readyCtx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("Deployd readiness timed out: %w", readyCtx.Err())
		case <-ticker.C:
		}
	}
}

func Apply(ctx context.Context, input Inputs) (Report, error) {
	if os.Geteuid() != 0 {
		return Report{}, errors.New("--apply requires local root")
	}
	// Every later consumer, including the legacy wrapper, uses only this private
	// copy. Preflight validates the bytes actually installed, not mutable paths.
	input, cleanup, err := snapshotInputs(input)
	if err != nil {
		return Report{}, err
	}
	defer cleanup()
	report, err := Preflight(ctx, input)
	if err != nil {
		return report, err
	}
	if os.Geteuid() != 0 {
		return report, errors.New("--apply requires local root")
	}
	if err := safeParents("/var/lib/agent-vm-native/deployment"); err != nil {
		return report, err
	}
	lock, err := native.AcquireLock("/var/lib/agent-vm-native/deployment/install.lock", true, true)
	if err != nil {
		return report, err
	}
	defer lock.Close()
	if report.Mode == "installed" {
		return finishExisting(ctx, report, input)
	}
	// Recheck after locking: a concurrent installer may have completed preflight.
	if _, err := os.Lstat("/var/lib/agent-vm-native/active"); !errors.Is(err, os.ErrNotExist) {
		return report, errors.New("installation changed after preflight; rerun read-only preflight")
	}
	cfg, err := LoadConfig(input.Config)
	if err != nil {
		return report, err
	}
	inputsHash, err := inputDigest(cfg)
	if err != nil {
		return report, err
	}
	r := record{SchemaVersion: 1, SourceRevision: report.SourceRevision, BundleSHA256: report.BundleSHA256, InputsSHA256: inputsHash, Phase: "files"}
	if data, err := os.ReadFile(recordPath); err == nil {
		if err := decodeStrict(data, &r); err != nil {
			return report, err
		}
		if r.SchemaVersion != 1 || r.SourceRevision != report.SourceRevision || r.BundleSHA256 != report.BundleSHA256 || r.InputsSHA256 != inputsHash {
			return report, errors.New("installation retry inputs conflict with protected journal")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		id := make([]byte, 16)
		if _, err := rand.Read(id); err != nil {
			return report, err
		}
		r.TransactionID = "install-" + hex.EncodeToString(id)
		if err := saveRecord(r); err != nil {
			return report, err
		}
	} else {
		return report, err
	}
	if r.Phase == "bootstrap_requested" || r.Phase == "migration_requested" {
		report.Next = "query agent-vm-deploy transaction --transaction " + r.TransactionID + "; resolve needs_recovery with recover before retrying installation"
		return report, errors.New("bootstrap was already requested; uncertain effects will not be replayed")
	}
	pins, err := components(input.Materials)
	if err != nil {
		return report, err
	}
	stage, err := os.MkdirTemp("/var/lib/agent-vm-native/deployment", ".install-stage-")
	if err != nil {
		return report, err
	}
	defer os.RemoveAll(stage)
	f, err := os.Open(input.Bundle)
	if err != nil {
		return report, err
	}
	product := filepath.Join(stage, "product")
	err = Extract(f, product, 1<<30)
	f.Close()
	if err != nil {
		return report, err
	}
	_, err = manifest.LoadMetadata(filepath.Join(product, "release-manifest.json"))
	if err != nil {
		return report, err
	}
	binaries := map[string]string{}
	for _, name := range []string{"caddy", "rqlite"} {
		binary, err := fetchComponent(ctx, name, pins[name], stage)
		if err != nil {
			return report, err
		}
		binaries[name] = binary
	}
	for _, name := range []string{"agent-vm-native.conf", "agent-vm-server.conf"} {
		target := filepath.Join("/etc/sysusers.d", name)
		if err := copyInstall(filepath.Join(input.Materials, "assets/sysusers.d", name), target, 0644, ""); err != nil {
			return report, err
		}
		if err := command(ctx, "systemd-sysusers", target); err != nil {
			return report, err
		}
	}
	if err := copyInstall(filepath.Join(input.Materials, "assets/tmpfiles.d/agent-vm-native.conf"), "/etc/tmpfiles.d/agent-vm-native.conf", 0644, ""); err != nil {
		return report, err
	}
	if err := command(ctx, "systemd-tmpfiles", "--create", "/etc/tmpfiles.d/agent-vm-native.conf"); err != nil {
		return report, err
	}
	for name, t := range targets {
		if cfg.Files[name] == "" {
			continue
		}
		data, err := protectedRead(cfg.Files[name])
		if err != nil {
			return report, err
		}
		if err := installFile(t.path, data, t.mode, t.group); err != nil {
			return report, err
		}
	}
	if err := installBackupInputs(cfg); err != nil {
		return report, err
	}
	if report.Mode != "legacy" {
		if err := installProductFiles(product, "/opt/agent-vm-native"); err != nil {
			return report, err
		}
	}

	for name, source := range binaries {
		binary := name
		if name == "rqlite" {
			binary = "rqlited"
		}
		if err := copyInstall(source, filepath.Join("/usr/local/bin", binary), 0755, ""); err != nil {
			return report, err
		}
	}
	for _, unit := range []string{
		"agent-vm-native@.service", "agent-vm-deployd.service", "agent-vm-rqlite.service", "agent-vm-caddy.service",
		"agent-vm-observe.socket", "agent-vm-observe@.service", "agent-vm-deploy-host.socket", "agent-vm-deploy-host@.service",
	} {
		source := filepath.Join(input.Materials, "assets/systemd", unit)
		if unit == "agent-vm-native@.service" || unit == "agent-vm-deployd.service" {
			source = filepath.Join(product, "systemd", unit)
		}
		if err := copyInstall(source, filepath.Join("/etc/systemd/system", unit), 0644, ""); err != nil {
			return report, err
		}
	}
	if err := copyInstall(input.Bundle, cachedBundle, 0600, ""); err != nil {
		return report, err
	}
	if hash, err := digestFile(cachedBundle); err != nil || hash != r.BundleSHA256 {
		return report, errors.New("cached bundle verification failed")
	}
	r.Phase = "services"
	if err := saveRecord(r); err != nil {
		return report, err
	}
	if err := command(ctx, "systemctl", "daemon-reload"); err != nil {
		return report, err
	}
	if err := command(ctx, "systemctl", "enable", "--now", "agent-vm-observe.socket", "agent-vm-deploy-host.socket"); err != nil {
		return report, err
	}
	if report.Mode == "legacy" {
		if err := command(ctx, "systemctl", "enable", "--now", "agent-vm-rqlite.service"); err != nil {
			return report, err
		}
		r.Phase = "migration_requested"
		if err := saveRecord(r); err != nil {
			return report, err
		}
		if err := command(ctx, "python3", filepath.Join(input.Materials, "scripts/agent_vm_release.py"), "install-local", "--bundle", cachedBundle, "--transaction-id", r.TransactionID); err != nil {
			report.Next = "inspect original wrapper transaction " + r.TransactionID + " and Deployd migrate/recover before retrying"
			return report, err
		}
		if err := command(ctx, "systemctl", "enable", "--now", "agent-vm-caddy.service"); err != nil {
			return report, err
		}
		r.Phase = "complete"
		if err := saveRecord(r); err != nil {
			return report, err
		}
		report.Mode = "installed"
		report.Next = "verify migrated state and VM lifecycle using deploy/docs/acceptance.md"
		return report, nil
	}
	if err := command(ctx, "systemctl", "enable", "--now", "agent-vm-rqlite.service", "agent-vm-deployd.service"); err != nil {
		return report, err
	}
	client := deployd.Client{Socket: "/run/agent-vm-native/deployd.sock", Timeout: 30 * time.Minute}
	if err := waitDaemon(ctx, client, 30*time.Second); err != nil {
		return report, err
	}
	// A running rqlite unit can still be electing a leader. Keep the install
	// journal at services until a read succeeds, so a timeout is safe to retry.
	blue, err := config.Load(targets["blue_config"].path)
	if err != nil {
		return report, errors.New("installed blue slot configuration is unavailable")
	}
	readyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err = state.WaitForRQLiteReady(readyCtx, blue.StateDB, targets["rqlite_certificate"].path)
	readyErr := readyCtx.Err()
	cancel()
	if err != nil {
		report.Next = "check agent-vm-rqlite.service and rerun install --apply with the same inputs"
		if readyErr != nil {
			return report, fmt.Errorf("rqlite was not ready before fresh bootstrap: %w", readyErr)
		}
		return report, errors.New("rqlite readiness probe configuration failed before fresh bootstrap")
	}
	r.Phase = "bootstrap_requested"
	if err := saveRecord(r); err != nil {
		return report, err
	}
	if _, _, err := client.Call(ctx, "POST", "/v1/bootstrap", map[string]any{"transaction_id": r.TransactionID, "bundle": cachedBundle}); err != nil {
		report.Next = "inspect transaction " + r.TransactionID + " with agent-vm-deploy transaction and recover"
		return report, err
	}
	if err := command(ctx, "systemctl", "enable", "agent-vm-native@blue.service"); err != nil {
		return report, err
	}
	if err := command(ctx, "systemctl", "enable", "--now", "agent-vm-caddy.service"); err != nil {
		return report, err
	}
	r.Phase = "complete"
	if err := saveRecord(r); err != nil {
		return report, err
	}
	report.Mode = "installed"
	report.Next = "verify VM lifecycle and Prometheus integration using deploy/docs/acceptance.md"
	return report, nil
}

func installBackupInputs(cfg Config) error {
	auth, err := protectedRead(cfg.Files["rqlite_auth"])
	if err != nil {
		return err
	}
	credential, err := deployd.ExportBackupCredential(auth)
	if err != nil {
		return err
	}
	ca, err := protectedRead(cfg.Files["rqlite_certificate"])
	if err != nil {
		return err
	}
	migration := deployd.Defaults().Migration
	if err := installFile(migration.RQLiteBackupAuth, credential, 0640, "agent-vm-deploy"); err != nil {
		return err
	}
	return installFile(migration.RQLiteBackupCA, ca, 0640, "agent-vm-deploy")
}

func finishExisting(ctx context.Context, report Report, input Inputs) (Report, error) {
	client := deployd.Client{Socket: "/run/agent-vm-native/deployd.sock", Timeout: 30 * time.Second}
	payload, _, err := client.Call(ctx, "GET", "/v1/status", nil)
	if err != nil {
		return report, errors.New("existing deployment observation is unavailable; no files changed")
	}
	var status struct {
		Value struct {
			Observation  deployd.RuntimeSnapshot `json:"observation"`
			Transactions []json.RawMessage       `json:"transactions"`
		} `json:"value"`
	}
	if err := json.Unmarshal(payload, &status); err != nil {
		return report, err
	}
	snapshot := status.Value.Observation
	active, ok := snapshot.Slots[snapshot.ActiveSlot]
	if len(status.Value.Transactions) != 0 || !ok || !snapshot.Consistent || !snapshot.FreshUntil.After(time.Now()) || snapshot.ControllerSlot != snapshot.ActiveSlot || !active.Ready || !active.ControllerOwned || !active.TrafficAccepting {
		return report, errors.New("existing installation is not verified ready; inspect Deployd status and recovery transactions")
	}
	data, err := protectedRead(recordPath)
	if errors.Is(err, os.ErrNotExist) {
		report.Next = "existing installation verified; use stage/verify/activate for upgrades"
		return report, nil
	}
	if err != nil {
		return report, err
	}
	var r record
	if err := decodeStrict(data, &r); err != nil {
		return report, err
	}
	if r.Phase != "complete" {
		cfg, err := LoadConfig(input.Config)
		if err != nil {
			return report, err
		}
		digest, err := inputDigest(cfg)
		if err != nil || digest != r.InputsSHA256 {
			return report, errors.New("unfinished installation protected inputs differ")
		}
		if r.SourceRevision != report.SourceRevision || r.BundleSHA256 != report.BundleSHA256 {
			return report, errors.New("unfinished installation inputs differ")
		}
		payload, _, err := client.Call(ctx, "GET", "/v1/transactions/"+r.TransactionID, nil)
		if err != nil {
			return report, err
		}
		var result struct {
			Value struct {
				Transaction struct {
					State string `json:"state"`
				} `json:"transaction"`
			} `json:"value"`
		}
		if err := json.Unmarshal(payload, &result); err != nil || result.Value.Transaction.State != "finalized" {
			return report, errors.New("original installation transaction is unresolved; use recover")
		}
		if err := command(ctx, "systemctl", "enable", "agent-vm-native@blue.service"); err != nil {
			return report, err
		}
		if err := command(ctx, "systemctl", "enable", "--now", "agent-vm-caddy.service"); err != nil {
			return report, err
		}
		r.Phase = "complete"
		if err := saveRecord(r); err != nil {
			return report, err
		}
	}
	report.Next = "existing installation verified; use stage/verify/activate for upgrades"
	return report, nil
}

// installProductFiles publishes only declared, verified files; the manifest is
// installed last so the fixed request adapter never accepts an incomplete tree.
func installProductFiles(product, destination string) error {
	if _, err := manifest.LoadMetadata(filepath.Join(product, "release-manifest.json")); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(product, "release-manifest.json"))
	if err != nil {
		return err
	}
	var release struct {
		Artifacts []struct {
			Path string `json:"path"`
			Mode uint32 `json:"mode"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(data, &release); err != nil {
		return err
	}
	for _, artifact := range release.Artifacts {
		if err := copyInstall(filepath.Join(product, filepath.FromSlash(artifact.Path)), filepath.Join(destination, filepath.FromSlash(artifact.Path)), os.FileMode(artifact.Mode), ""); err != nil {
			return err
		}
	}
	// The client sidecar is derived metadata, not a manifest artifact. Verify it
	// against the already-verified manifest before publishing either metadata file.
	sidecar, err := verifiedProductSidecar(product, data)
	if err != nil {
		return err
	}
	if sidecar != nil {
		if err := installFile(filepath.Join(destination, "client-release.json"), sidecar, 0644, ""); err != nil {
			return err
		}
	}
	return copyInstall(filepath.Join(product, "release-manifest.json"), filepath.Join(destination, "release-manifest.json"), 0644, "")
}

func verifiedProductSidecar(product string, manifestBytes []byte) ([]byte, error) {
	var release map[string]any
	if err := json.Unmarshal(manifestBytes, &release); err != nil {
		return nil, err
	}
	artifacts, _ := release["artifacts"].([]any)
	var client any
	dependencies := []any{}
	for _, item := range artifacts {
		artifact, ok := item.(map[string]any)
		if !ok {
			return nil, errors.New("invalid product artifact")
		}
		if artifact["role"] == "client" {
			client = item
		}
		if artifact["role"] == "support" {
			dependencies = append(dependencies, item)
		}
	}
	if client == nil {
		return nil, nil
	}
	data, err := os.ReadFile(filepath.Join(product, "client-release.json"))
	if err != nil {
		return nil, err
	}
	var actual map[string]any
	if err := json.Unmarshal(data, &actual); err != nil {
		return nil, err
	}
	expected := map[string]any{"format": release["format"], "version": release["version"], "source_revision": release["source_revision"], "protocol": release["protocol"], "client_artifact": client, "client_dependencies": dependencies, "client_view": release["client_view"]}
	if !reflect.DeepEqual(actual, expected) {
		return nil, errors.New("client sidecar does not match verified product manifest")
	}
	return data, nil
}
