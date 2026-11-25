package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/config"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/manifest"
)

// MaintenanceBinaryPath is the stable deployd executable outside the slots. A
// blue-green update replaces slot contents only, so the installed stable binary
// must be replaced and its service restarted by a separate host action.
const MaintenanceBinaryPath = migrationDeployBinary

// DeployBinaryPaths are the two operator entrypoints for the same binary. The
// maintenance copy is what systemd runs; the bin copy is what operators call,
// so both must carry the release that the kept backup and restart describe.
var DeployBinaryPaths = []string{migrationDeployBinary, "/opt/agent-vm-native/bin/agent-vm-deploy"}

var deploydVersionPattern = regexp.MustCompile(`agent-vm-deploy/[0-9][0-9A-Za-z.+-]*`)

// ServedDeployBinaryVersion reports the version of the installed stable binary.
func ServedDeployBinaryVersion() (string, error) {
	output, err := exec.Command(MaintenanceBinaryPath, "version").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

// DeployBinaryVersion reads a binary's self-reported version without running it.
func DeployBinaryVersion(path string) (string, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	match := deploydVersionPattern.Find(contents)
	if match == nil {
		return "", errors.New("binary does not report an agent-vm-deploy version")
	}
	return string(match), nil
}

// InstallMaintenanceBinary replaces the stable deployd executable with the
// release copy, keeping one reversible backup under the same version-free name.
// The caller owns restarting agent-vm-deployd.service.
func InstallMaintenanceBinary(product string) ([]string, error) {
	return installDeployBinary(product, copyInstall)
}

// installDeployBinary takes the copy function so the path set and backup
// semantics can be tested without privileged parent directories.
func installDeployBinary(product string, copy func(string, string, os.FileMode, string) error) ([]string, error) {
	source := filepath.Join(product, "bin", "agent-vm-deploy")
	var backups []string
	for _, target := range DeployBinaryPaths {
		staged := target + ".new"
		if err := copy(source, staged, 0755, ""); err != nil {
			return backups, err
		}
		backup := target + ".previous"
		if err := os.Rename(target, backup); err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = os.Remove(staged)
			return backups, err
		}
		if err := os.Rename(staged, target); err != nil {
			_ = os.Rename(backup, target)
			return backups, err
		}
		backups = append(backups, backup)
	}
	return backups, nil
}

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
	// The migration drop-in starts the candidate as agent-vm-server. Open the
	// incumbent tree first so a failed candidate can be restarted by the same
	// drop-in during the coordinator's stage compensation.
	if err := PrepareLegacySlotAccess(activeSlot); err != nil {
		return fmt.Errorf("prepare incumbent slot access for account migration: %w", err)
	}
	// The maintenance binary is replaced, not merely created: an older install
	// already owns this path and its bytes cannot match the candidate release.
	// Reuse the atomic staged-rename contract so the previous binary survives as
	// .previous for recovery.
	if _, err := installDeployBinary(product, copyInstall); err != nil {
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
	if err := prepareRequestEntry(ctx, materials, true); err != nil {
		return err
	}
	if err := prepareDeploydForCandidate(ctx); err != nil {
		return err
	}
	if err := prepareRecoveryCredentials(); err != nil {
		return err
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

// prepareRecoveryCredentials supplies the backup-only rqlite identity that
// recovery points need. Deployd releases before the recovery feature never
// created these files, and stage refuses without them.
func prepareRecoveryCredentials() error {
	const (
		authDir  = "/etc/agent-vm-native/backup"
		authPath = authDir + "/auth.json"
		caPath   = authDir + "/ca.crt"
	)
	installedAuth, err := protectedRead("/etc/agent-vm-native/rqlite/auth.json")
	if err != nil {
		return err
	}
	credential, err := deployd.ExportBackupCredential(installedAuth)
	if err != nil {
		return err
	}
	certificate, err := protectedRead("/etc/agent-vm-native/tls/rqlite/server.crt")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(authDir, 0750); err != nil {
		return err
	}
	if err := os.Chown(authDir, 0, gidOf("agent-vm-deploy")); err != nil {
		return err
	}
	for path, data := range map[string][]byte{authPath: credential, caPath: certificate} {
		if existing, readErr := os.ReadFile(path); readErr == nil && bytes.Equal(existing, data) {
			continue
		}
		if err := replaceOwnedFileOrCreate(path, data, 0640); err != nil {
			return err
		}
		if err := os.Chown(path, 0, gidOf("agent-vm-deploy")); err != nil {
			return err
		}
	}
	return nil
}

func gidOf(group string) int {
	found, err := user.LookupGroup(group)
	if err != nil {
		return 0
	}
	value, err := strconv.Atoi(found.Gid)
	if err != nil {
		return 0
	}
	return value
}

func replaceOwnedFileOrCreate(path string, data []byte, mode os.FileMode) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return installFile(path, data, mode, "")
	}
	return replaceOwnedFile(path, data, mode)
}

// prepareDeploydForCandidate moves the incumbent root deployd onto the verified
// candidate executable before staging. Legacy units run an older deployd that
// cannot write the candidate slot drop-ins (ProtectSystem=full without the
// drop-in directories in ReadWritePaths), so the unit is widened and the
// candidate binary replaces the maintenance copy first.
func prepareDeploydForCandidate(ctx context.Context) error {
	for _, slot := range []string{"blue", "green"} {
		dir := filepath.Join("/etc/systemd/system", "agent-vm-native@"+slot+".service.d")
		if err := os.MkdirAll(dir, 0750); err != nil {
			return err
		}
	}
	path := "/etc/systemd/system/agent-vm-deployd.service"
	current, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	next, err := migratedDeploydUnit(current)
	if err != nil {
		return err
	}
	if err := replaceOwnedFile(path, next, 0644); err != nil {
		return err
	}
	if err := command(ctx, "/usr/bin/systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := command(ctx, "/usr/bin/systemctl", "restart", "agent-vm-deployd.service"); err != nil {
		return err
	}
	return waitDeploydStatus(ctx)
}

// migratedDeploydUnit points the stable service at the verified maintenance copy
// and lets it write the candidate drop-ins. It is idempotent for retries.
func migratedDeploydUnit(current []byte) ([]byte, error) {
	value := string(current)
	started := false
	lines := strings.Split(value, "\n")
	for index, line := range lines {
		if strings.HasPrefix(line, "ExecStart=/opt/agent-vm-native/") {
			lines[index] = "ExecStart=" + migrationDeployBinary + " serve --config /etc/agent-vm-native/deployd.json"
			started = true
		}
		if strings.HasPrefix(line, "ReadWritePaths=") {
			for _, dir := range []string{
				"/etc/systemd/system/agent-vm-native@blue.service.d",
				"/etc/systemd/system/agent-vm-native@green.service.d",
			} {
				if !strings.Contains(line, dir) {
					lines[index] += " " + dir
				}
			}
		}
	}
	if !started {
		return nil, errors.New("deployd unit does not start a verified deploy executable")
	}
	value = strings.Join(lines, "\n")
	if !strings.Contains(value, "ReadWritePaths=") {
		value = strings.Replace(value, "[Service]\n", "[Service]\nReadWritePaths=/var/lib/agent-vm-native /run/agent-vm-native /etc/systemd/system/agent-vm-native@blue.service.d /etc/systemd/system/agent-vm-native@green.service.d\n", 1)
	}
	return []byte(value), nil
}

// replaceOwnedFile updates a root-owned unit in place. Unlike installFile it
// accepts changed bytes, because the candidate unit is a legitimate rewrite.
func replaceOwnedFile(path string, data []byte, mode os.FileMode) error {
	info, err := os.Lstat(path)
	owner, owned := info.Sys().(*syscall.Stat_t)
	if err != nil || !owned || owner.Uid != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("refusing to replace %s", path)
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".unit-")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Chmod(mode); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func waitDeploydStatus(ctx context.Context) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		probe, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, _, err := deployd.Client{Socket: deployd.Defaults().Socket, Timeout: 5 * time.Second}.Call(probe, "GET", "/v1/status", nil)
		cancel()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("candidate deployd did not answer after restart")
		}
		time.Sleep(time.Second)
	}
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
	// Either slot can receive a historical release in a later update.
	mounts := make(map[string][]byte, 2)
	for _, slot := range []string{rollback, active} {
		mount, err := installedSlotLockMount(slot)
		if err != nil {
			return fmt.Errorf("%s slot: %w", slot, err)
		}
		mounts[slot] = mount
	}
	for _, slot := range []string{rollback, active} {
		dir := filepath.Join("/etc/systemd/system", "agent-vm-native@"+slot+".service.d")
		content := append(append(bytes.Clone(account), '\n'), mounts[slot]...)
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

func installedSlotLockMount(slot string) ([]byte, error) {
	current, err := config.Load(targets[slot+"_config"].path)
	if err != nil {
		return nil, err
	}
	return legacySlotLockMount(current.StoragePath)
}

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
	// Legacy installations ship the observer socket disabled; recovery and the
	// controller lease maintenance both need it after the migration restarts
	// deployd.
	return command(ctx, "/usr/bin/systemctl", "enable", "--now", socket)
}

func migrationObserverUnit(service []byte) ([]byte, error) {
	return migrationHelperUnit(service, "host-observe")
}

func migrationHelperUnit(service []byte, action string) ([]byte, error) {
	switch action {
	case "host-observe", "host-action", "slot-relay", "request-relay":
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
	//
	// restart, not start: a legacy install may already run these sockets with
	// the old definitions, and systemd drops the listening file descriptor once
	// a socket unit's configuration changed under it. `start` is a no-op there
	// and leaves a unit that reports active while every connect is refused.
	if err := command(ctx, "/usr/bin/systemctl", "enable", "agent-vm-deploy-host.socket", "agent-vm-slot-relay.socket"); err != nil {
		return err
	}
	return command(ctx, "/usr/bin/systemctl", "restart", "agent-vm-deploy-host.socket", "agent-vm-slot-relay.socket")
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
	if err := command(ctx, "systemd-tmpfiles", "--create", target); err != nil {
		return err
	}
	return prepareRequestEntry(ctx, materials, false)
}

// Publish the root-only SSH entry before starting the candidate. Historical
// request binaries continue to work; neither their files nor their manifest
// need to be rewritten during account migration.
func prepareRequestEntry(ctx context.Context, materials string, migration bool) error {
	if _, err := os.Lstat(deployd.RequestRelaySocket); err == nil {
		if err := command(ctx, "/usr/bin/systemctl", "is-active", "--quiet", "agent-vm-request.socket"); err != nil {
			return errors.New("SSH request socket is already occupied; retain the current service")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	units := filepath.Join(materials, "assets/systemd")
	service, err := os.ReadFile(filepath.Join(units, "agent-vm-request@.service"))
	if err != nil {
		return err
	}
	if migration {
		service, err = migrationHelperUnit(service, "request-relay")
		if err != nil {
			return err
		}
	}
	if err := installFile("/etc/systemd/system/agent-vm-request@.service", service, 0644, ""); err != nil {
		return err
	}
	if err := copyInstall(filepath.Join(units, "agent-vm-request.socket"), "/etc/systemd/system/agent-vm-request.socket", 0644, ""); err != nil {
		return err
	}
	if err := command(ctx, "/usr/bin/systemctl", "daemon-reload"); err != nil {
		return err
	}
	return command(ctx, "/usr/bin/systemctl", "enable", "--now", "agent-vm-request.socket")
}

func stoppedAccountSlot(status string) bool {
	fields := strings.Fields(status)
	return len(fields) == 2 && ((fields[0] == "MainPID=0" && fields[1] == "ActiveState=inactive") || (fields[1] == "MainPID=0" && fields[0] == "ActiveState=inactive"))
}
