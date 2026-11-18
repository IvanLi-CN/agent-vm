package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/config"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/manifest"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

// PreflightAccountMigration checks the active release that would become the
// rollback target. It does not stop services, acquire roles or write files.
func PreflightAccountMigration(ctx context.Context, input Inputs) (Report, error) {
	report, err := Preflight(ctx, input)
	if err != nil {
		return report, err
	}
	report.Next = "retain the current slot until host account migration can proceed"
	fail := func(err error) (Report, error) {
		report.Blockers = append(report.Blockers, err.Error())
		return report, errors.New("host account migration preflight blocked")
	}
	if report.Mode != "installed" {
		return fail(errors.New("host account migration requires an installed blue-green deployment"))
	}
	cfg, err := LoadConfig(input.Config)
	if err != nil {
		return fail(err)
	}
	paths := deployd.Defaults()
	client := deployd.Client{Socket: paths.Socket, Timeout: 30 * time.Second}
	payload, _, err := client.Call(ctx, "GET", "/v1/status", nil)
	if err != nil {
		return fail(errors.New("cannot read current deployment status"))
	}
	snapshot, active, err := accountMigrationActive(payload, time.Now())
	if err != nil {
		return fail(err)
	}
	report.ActiveSlot = snapshot.ActiveSlot
	slotRoot := filepath.Join(paths.ReleaseRoot, "slots", snapshot.ActiveSlot)
	activePath, err := deployd.EffectiveActivePath(paths.ActivePath)
	if err != nil || validateAccountMigrationRoute(activePath, slotRoot) != nil {
		return fail(errors.New("active route does not match the observed slot"))
	}
	metadata, err := manifest.LoadMetadata(filepath.Join(slotRoot, "release-manifest.json"))
	if err != nil || !metadata.ArtifactsVerified || metadata.SourceRevision != active.SourceRevision || metadata.ReleaseID != active.ReleaseID {
		return fail(errors.New("active release artifacts do not match the live process identity"))
	}
	report.PreviousSource = metadata.SourceRevision
	for _, slot := range []string{"blue", "green"} {
		if err := checkAccountMigrationConfig(slot, cfg.Files[slot+"_config"], active.StateAuthority); err != nil {
			return fail(err)
		}
	}
	if err := checkInstalledBackupReader(ctx, cfg, paths.Migration); err != nil {
		return fail(err)
	}
	report.Next = "bind isolated rollback compatibility evidence to the reported previous source revision and candidate bundle digest before migration"
	return report, nil
}

func checkInstalledBackupReader(ctx context.Context, cfg Config, paths deployd.MigrationConfig) error {
	installed, err := protectedRead(paths.RQLiteAuthFile)
	if err != nil {
		return errors.New("cannot read the protected rqlite authentication file on the installed host")
	}
	provided, err := protectedRead(cfg.Files["rqlite_auth"])
	if err != nil {
		return errors.New("cannot read the supplied rqlite authentication file")
	}
	credential, err := matchingBackupCredential(installed, provided)
	if err != nil {
		return err
	}
	ca, err := protectedRead(cfg.Files["rqlite_certificate"])
	if err != nil {
		return errors.New("cannot read the supplied rqlite TLS certificate")
	}
	return deployd.ProbeInstalledBackupReader(ctx, ca, credential)
}

func matchingBackupCredential(installed, supplied []byte) ([]byte, error) {
	current, currentErr := deployd.ExportBackupCredential(installed)
	input, inputErr := deployd.ExportBackupCredential(supplied)
	if currentErr != nil || inputErr != nil || !bytes.Equal(current, input) {
		return nil, errors.New("installed rqlite and supplied inputs do not have matching restricted backup credentials")
	}
	return current, nil
}

func accountMigrationActive(data []byte, now time.Time) (deployd.RuntimeSnapshot, deployd.SlotObservation, error) {
	var result struct {
		OK    bool `json:"ok"`
		Value struct {
			Observed     deployd.RuntimeSnapshot `json:"observation"`
			Transactions []json.RawMessage       `json:"transactions"`
			Dedicated    bool                    `json:"dedicated_service_account"`
		} `json:"value"`
	}
	if json.Unmarshal(data, &result) != nil || !result.OK {
		return deployd.RuntimeSnapshot{}, deployd.SlotObservation{}, errors.New("deployment status is invalid")
	}
	if result.Value.Dedicated {
		return deployd.RuntimeSnapshot{}, deployd.SlotObservation{}, errors.New("deployment already uses the dedicated service account")
	}
	snapshot := result.Value.Observed
	active := snapshot.Slots[snapshot.ActiveSlot]
	other := "blue"
	if snapshot.ActiveSlot == "blue" {
		other = "green"
	}
	if len(result.Value.Transactions) != 0 || (snapshot.ActiveSlot != "blue" && snapshot.ActiveSlot != "green") ||
		!snapshot.Consistent || !snapshot.FreshUntil.After(now) || snapshot.ControllerSlot != snapshot.ActiveSlot ||
		!active.ProcessUp || !active.Ready || !active.ControllerOwned || !active.TrafficAccepting || active.Draining ||
		active.StateAuthority == "" || active.SourceRevision == "" || active.ReleaseID == "" ||
		snapshot.Slots[other].ControllerOwned || snapshot.Slots[other].TrafficAccepting {
		return snapshot, active, errors.New("existing deployment is not ready for account migration")
	}
	return snapshot, active, nil
}

func checkAccountMigrationConfig(slot, input, activeAuthority string) error {
	path := filepath.Join("/etc/agent-vm-native/slots", slot, "config.json")
	if _, err := protectedRead(path); err != nil {
		return fmt.Errorf("installed %s configuration is not protected", slot)
	}
	current, currentErr := config.Load(path)
	next, nextErr := config.Load(input)
	if currentErr != nil || nextErr != nil || !sameAccountMigrationEnvironment(slot, current, next) {
		return fmt.Errorf("host account migration would change the %s slot state authority or Incus environment", slot)
	}
	if !matchesLiveAccountMigrationAuthority(next.StateDB, activeAuthority) {
		return fmt.Errorf("configured %s slot rqlite authority does not match the live service", slot)
	}
	return nil
}

func matchesLiveAccountMigrationAuthority(dsn, observed string) bool {
	if observed == "" || config.ValidateInstallationAuthority(dsn) != nil {
		return false
	}
	endpoint, err := url.Parse(dsn)
	return err == nil && state.RQLiteAuthorityIdentity(*endpoint) == observed
}

func sameAccountMigrationEnvironment(slot string, current, next config.Config) bool {
	return current.Slot == slot && next.Slot == slot && current.Socket == next.Socket &&
		current.StateDB == next.StateDB && current.Project == next.Project && current.Profile == next.Profile &&
		current.Network == next.Network && current.Pool == next.Pool && current.StoragePath == next.StoragePath &&
		current.ImageFingerprint == next.ImageFingerprint
}

func validateAccountMigrationRoute(path, expected string) error {
	target, err := os.Readlink(path)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(path), target)
	}
	if target != expected {
		return errors.New("active route targets another slot")
	}
	return validateSlotRoute(target, filepath.Dir(expected))
}
