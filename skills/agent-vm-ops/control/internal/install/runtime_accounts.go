package install

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/manifest"
)

const migrationDeployBinary = "/opt/agent-vm-native/maintenance/agent-vm-deploy"

// prepareLegacyCandidateUnit is called with privately staged and validated
// installation inputs under the release-update lock, before the incumbent daemon
// receives a stage request.
func prepareLegacyCandidateUnit(ctx context.Context, activeSlot string, input Inputs, product, revision string) error {
	materials := input.Materials
	var candidate string
	switch activeSlot {
	case "blue":
		candidate = "green"
	case "green":
		candidate = "blue"
	default:
		return errors.New("account migration requires a known active slot")
	}
	meta, err := manifest.LoadMetadata(filepath.Join(product, "release-manifest.json"))
	if err != nil || !meta.ArtifactsVerified || meta.SourceRevision != revision {
		return errors.New("migration maintenance executable does not match the candidate release")
	}
	defaults := deployd.Defaults()
	route, err := deployd.EffectiveActivePath(defaults.ActivePath)
	if err != nil || validateAccountMigrationRoute(route, filepath.Join(defaults.ReleaseRoot, "slots", activeSlot)) != nil {
		return errors.New("active slot changed before preparing the migration candidate")
	}
	unit := "agent-vm-native@" + candidate + ".service"
	status, err := exec.CommandContext(ctx, "/usr/bin/systemctl", "show", unit, "--property=ActiveState", "--property=MainPID").Output()
	if err != nil || !stoppedAccountSlot(string(status)) {
		return errors.New("account migration requires the candidate slot to be stopped")
	}
	if err := prepareCandidateAccess(ctx, candidate, input); err != nil {
		return err
	}
	if err := copyInstall(filepath.Join(product, "bin", "agent-vm-deploy"), migrationDeployBinary, 0755, ""); err != nil {
		return err
	}
	if err := prepareMigrationObserver(ctx, materials); err != nil {
		return err
	}
	dir := filepath.Join("/etc/systemd/system", unit+".d")
	account, err := os.ReadFile(filepath.Join(materials, "assets/systemd", "fresh-slot-account.conf"))
	if err != nil {
		return err
	}
	access, err := os.ReadFile(filepath.Join(materials, "assets/systemd", "legacy-slot-access.conf"))
	if err != nil {
		return err
	}
	dropIn := append(append(account, '\n'), access...)
	if err := installFile(filepath.Join(dir, "19-account-migration.conf"), dropIn, 0644, ""); err != nil {
		return err
	}
	return command(ctx, "/usr/bin/systemctl", "daemon-reload")
}

func prepareMigrationObserver(ctx context.Context, materials string) error {
	units := filepath.Join(materials, "assets/systemd")
	const observer = "agent-vm-observe@.service"
	const socket = "agent-vm-observe.socket"
	const systemd = "/etc/systemd/system"
	service, err := os.ReadFile(filepath.Join(units, observer))
	if err != nil {
		return err
	}
	service, err = migrationObserverUnit(service)
	if err != nil {
		return err
	}
	if err := installFile(filepath.Join(systemd, observer), service, 0644, ""); err != nil {
		return err
	}
	if err := copyInstall(filepath.Join(units, socket), filepath.Join(systemd, socket), 0644, ""); err != nil {
		return err
	}
	if err := command(ctx, "/usr/bin/systemctl", "daemon-reload"); err != nil {
		return err
	}
	return command(ctx, "/usr/bin/systemctl", "start", socket)
}

func migrationObserverUnit(service []byte) ([]byte, error) {
	original := []byte("\nExecStart=/opt/agent-vm-native/bin/agent-vm-deploy host-observe\n")
	if bytes.Count(service, original) != 1 {
		return nil, errors.New("read-only host observation service does not use the expected command")
	}
	return bytes.Replace(service, original, []byte("\nExecStart="+migrationDeployBinary+" host-observe\n"), 1), nil
}

func prepareFreshAccounts(ctx context.Context, materials string) error {
	if err := command(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	for _, slot := range []string{"blue", "green"} {
		status, err := exec.CommandContext(ctx, "systemctl", "show", "agent-vm-native@"+slot+".service", "--property=ActiveState", "--property=MainPID").Output()
		if err != nil || !stoppedAccountSlot(string(status)) {
			return errors.New("fresh account setup requires both slots stopped")
		}
	}
	if err := installFile("/opt/agent-vm-native/compat-incus/incus", []byte("#!/bin/sh\nexec /opt/agent-vm-native/bin/agent-vm-deploy legacy-incus \"$AGENT_VM_LEGACY_SLOT\" \"$@\"\n"), 0755, ""); err != nil {
		return err
	}
	for _, unit := range []string{"agent-vm-native@blue.service", "agent-vm-native@green.service", "agent-vm-deployd.service"} {
		name := "fresh-slot-account.conf"
		if unit == "agent-vm-deployd.service" {
			name = "fresh-deploy-account.conf"
		}
		if err := copyInstall(filepath.Join(materials, "assets/systemd", name), filepath.Join("/etc/systemd/system", unit+".d", "20-service-account.conf"), 0644, ""); err != nil {
			return err
		}
	}
	if err := command(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	const name = "agent-vm-fresh-accounts.conf"
	target := filepath.Join("/etc/tmpfiles.d", name)
	if err := copyInstall(filepath.Join(materials, "assets/tmpfiles.d", name), target, 0644, ""); err != nil {
		return err
	}
	return command(ctx, "systemd-tmpfiles", "--create", target)
}

func stoppedAccountSlot(status string) bool {
	fields := strings.Fields(status)
	return len(fields) == 2 && ((fields[0] == "MainPID=0" && fields[1] == "ActiveState=inactive") || (fields[1] == "MainPID=0" && fields[0] == "ActiveState=inactive"))
}
