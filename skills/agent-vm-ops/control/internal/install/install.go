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
	"sort"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/manifest"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/native"
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
func waitDaemon(ctx context.Context, client deployd.Client) error {
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, _, err := client.Call(ctx, "GET", "/v1/status", nil); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return errors.New("Deployd readiness timed out")
		case <-ticker.C:
		}
	}
}

func Apply(ctx context.Context, input Inputs) (Report, error) {
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
	if _, err := manifest.LoadMetadata(filepath.Join(product, "release-manifest.json")); err != nil {
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
	if err := copyInstall(filepath.Join(input.Materials, "assets/sysusers.d/agent-vm-native.conf"), "/etc/sysusers.d/agent-vm-native.conf", 0644, ""); err != nil {
		return report, err
	}
	if err := command(ctx, "systemd-sysusers", "/etc/sysusers.d/agent-vm-native.conf"); err != nil {
		return report, err
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
	for _, name := range []string{"agent-vm-deployd", "agent-vm-deploy"} {
		if report.Mode == "legacy" {
			continue
		}
		if err := copyInstall(filepath.Join(product, "bin", name), filepath.Join("/opt/agent-vm-native/bin", name), 0755, ""); err != nil {
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
	for _, unit := range []string{"agent-vm-native@.service", "agent-vm-deployd.service", "agent-vm-rqlite.service", "agent-vm-caddy.service"} {
		if err := copyInstall(filepath.Join(input.Materials, "assets/systemd", unit), filepath.Join("/etc/systemd/system", unit), 0644, ""); err != nil {
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
	if err := waitDaemon(ctx, client); err != nil {
		return report, err
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
