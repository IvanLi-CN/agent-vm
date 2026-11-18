package install

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/config"
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
	// The legacy installation creates these root-owned directories with mode
	// 0750. Permit traversal to the verified helper without opening its old bin.
	for _, dir := range []string{"/opt/agent-vm-native", filepath.Dir(migrationDeployBinary)} {
		if err := safeParents(dir); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0751); err != nil {
			return err
		}
	}
	if err := prepareMigrationObserver(ctx, materials); err != nil {
		return err
	}
	dir := filepath.Join("/etc/systemd/system", unit+".d")
	_, dropIn, err := slotAccountDropIns(materials)
	if err != nil {
		return err
	}
	if err := installFile(filepath.Join(dir, "19-account-migration.conf"), dropIn, 0644, ""); err != nil {
		return err
	}
	return command(ctx, "/usr/bin/systemctl", "daemon-reload")
}

func slotAccountDropIns(materials string) (account, migration []byte, err error) {
	account, err = os.ReadFile(filepath.Join(materials, "assets/systemd", "fresh-slot-account.conf"))
	if err != nil {
		return nil, nil, err
	}
	access, err := os.ReadFile(filepath.Join(materials, "assets/systemd", "legacy-slot-access.conf"))
	if err != nil {
		return nil, nil, err
	}
	pre := []byte("ExecStartPre=" + migrationDeployBinary + " wait-authority --slot %i --timeout 55\n")
	post := []byte("ExecStartPost=\nExecStartPost=" + migrationDeployBinary + " wait-process --slot %i --timeout 55\n")
	steady := append(append(bytes.Clone(account), []byte("\n[Service]\nExecStartPre=\n")...), pre...)
	steady = append(steady, post...)
	during := append(append(bytes.Clone(account), '\n'), access...)
	during = append(during, pre...)
	return steady, append(during, post...), nil
}

// finalizeSlotAccounts is called after successful activation, while the old
// coordinator still owns the live lease and deployment updates are excluded.
func finalizeSlotAccounts(ctx context.Context, input Inputs, active string) error {
	var rollback string
	switch active {
	case "blue":
		rollback = "green"
	case "green":
		rollback = "blue"
	default:
		return errors.New("slot account finalization requires a verified active slot")
	}
	cfg := deployd.Defaults()
	route, err := deployd.EffectiveActivePath(cfg.ActivePath)
	if err != nil || validateAccountMigrationRoute(route, filepath.Join(cfg.ReleaseRoot, "slots", active)) != nil {
		return errors.New("active route changed before account finalization")
	}
	stopped, err := exec.CommandContext(ctx, "/usr/bin/systemctl", "show", "agent-vm-native@"+rollback+".service", "--property=ActiveState", "--property=MainPID").Output()
	if err != nil || !stoppedAccountSlot(string(stopped)) {
		return errors.New("old slot must be stopped before applying its account configuration")
	}
	if err := prepareCandidateAccess(ctx, rollback, input); err != nil {
		return err
	}
	if err := PrepareLegacySlotAccess(rollback); err != nil {
		return err
	}
	account, migration, err := slotAccountDropIns(input.Materials)
	if err != nil {
		return err
	}
	previous, err := config.Load(targets[rollback+"_config"].path)
	if err != nil {
		return err
	}
	legacyMount, err := legacySlotLockMount(previous.StoragePath)
	if err != nil {
		return err
	}
	for _, slot := range []string{rollback, active} {
		dir := filepath.Join("/etc/systemd/system", "agent-vm-native@"+slot+".service.d")
		content := account
		if slot == rollback {
			content = append(append(bytes.Clone(account), '\n'), legacyMount...)
		}
		if err := installFile(filepath.Join(dir, "20-service-account.conf"), content, 0644, ""); err != nil {
			return err
		}
	}
	dir := filepath.Join("/etc/systemd/system", "agent-vm-native@"+active+".service.d")
	if err := removeAccountHandoffDropIn(filepath.Join(dir, "19-account-migration.conf"), migration); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := command(ctx, "/usr/bin/systemctl", "daemon-reload"); err != nil {
		return err
	}
	return deployd.VerifyUnprivilegedSlots(ctx)
}

var systemdStoragePath = regexp.MustCompile(`^(/[A-Za-z0-9._-]+)+$`)

func legacySlotLockMount(storagePath string) ([]byte, error) {
	if !systemdStoragePath.MatchString(storagePath) || filepath.Clean(storagePath) != storagePath {
		return nil, errors.New("legacy slot requires a literal absolute Incus storage path")
	}
	target := filepath.Join(storagePath, "locks")
	check := target
	if _, err := os.Lstat(target); errors.Is(err, os.ErrNotExist) {
		check = storagePath
	}
	for path := check; ; path = filepath.Dir(path) {
		info, err := os.Lstat(path)
		if err != nil || !rootProtected(info, 0) || !info.IsDir() {
			return nil, errors.New("legacy slot lock mount requires protected storage directories")
		}
		if path == "/" {
			break
		}
	}
	// Old releases derive the lock path from the pool. The private mount lets
	// both releases share the same lock files without giving the slot pool access.
	return []byte("BindPaths=" + deployd.ServerLockDir + ":" + target + "\n"), nil
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
	return migrationHelperUnit(service, "host-observe")
}

func migrationHelperUnit(service []byte, action string) ([]byte, error) {
	switch action {
	case "host-observe", "host-action", "slot-relay":
	default:
		return nil, errors.New("invalid migration helper action")
	}
	original := []byte("\nExecStart=/opt/agent-vm-native/bin/agent-vm-deploy " + action + "\n")
	if bytes.Count(service, original) != 1 {
		return nil, errors.New("migration helper service does not use the expected command")
	}
	return bytes.Replace(service, original, []byte("\nExecStart="+migrationDeployBinary+" "+action+"\n"), 1), nil
}

func prepareHandoffSockets(ctx context.Context, materials string) error {
	units := filepath.Join(materials, "assets/systemd")
	const systemd = "/etc/systemd/system"
	for _, helper := range []struct{ name, action string }{
		{"agent-vm-deploy-host@.service", "host-action"},
		{"agent-vm-slot-relay@.service", "slot-relay"},
	} {
		content, err := os.ReadFile(filepath.Join(units, helper.name))
		if err != nil {
			return err
		}
		content, err = migrationHelperUnit(content, helper.action)
		if err != nil {
			return err
		}
		if err := installFile(filepath.Join(systemd, helper.name), content, 0644, ""); err != nil {
			return err
		}
	}
	for _, filename := range []string{"agent-vm-deploy-host.socket", "agent-vm-slot-relay.socket", "agent-vm-deployd.socket"} {
		if err := copyInstall(filepath.Join(units, filename), filepath.Join(systemd, filename), 0644, ""); err != nil {
			return err
		}
	}
	if err := command(ctx, "/usr/bin/systemctl", "daemon-reload"); err != nil {
		return err
	}
	// The API socket still belongs to the old daemon. It is pulled in by
	// the new service's Requires= only after the old process has stopped.
	return command(ctx, "/usr/bin/systemctl", "start", "agent-vm-deploy-host.socket", "agent-vm-slot-relay.socket")
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
