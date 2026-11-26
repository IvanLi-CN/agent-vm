package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/controllerrole"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/native"
)

const deploymentUnit = "agent-vm-deployd.service"

const deploydSocketUnit = "agent-vm-deployd.socket"

// handoffDeploymentUnit keeps the incumbent unit definition in place up to
// the point the journal transfer succeeds. The passed materials are from the
// verified, private installation snapshot.
func handoffDeploymentUnit(ctx context.Context, handoff deployd.AccountHandoff, input Inputs) (result error) {
	materials := input.Materials
	account, err := os.ReadFile(filepath.Join(materials, "assets/systemd/fresh-deploy-account.conf"))
	if err != nil {
		return err
	}
	if err := prepareHandoffAccount(ctx, input); err != nil {
		return err
	}
	if err := prepareHandoffSockets(ctx, materials); err != nil {
		return err
	}
	if err := finalizeSlotAccounts(ctx, input, handoff.Lease.Slot); err != nil {
		return err
	}
	steady, adopt := deploymentAccountDropIns(account)
	const directory = "/etc/systemd/system/" + deploymentUnit + ".d"
	active := filepath.Join(directory, "20-service-account.conf")
	temporary := filepath.Join(directory, "21-account-handoff.conf")
	published := false
	defer func() {
		// The startup-only instruction must never outlive the attempt. A
		// --adopt-account-handoff that survives into the steady unit makes every
		// later restart crash once the witness lease expires, so it is cleared on
		// every outcome, including a failed ownership confirmation.
		clearCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if published {
			if err := removeAccountHandoffDropIn(temporary, adopt); err != nil {
				result = errors.Join(result, fmt.Errorf("startup-only unit configuration was not cleared: %w", err))
			} else if err := command(clearCtx, "/usr/bin/systemctl", "daemon-reload"); err != nil {
				result = errors.Join(result, fmt.Errorf("startup-only unit configuration reload failed: %w", err))
			}
		}
		// The evidence is single-use and is published inside the switch, before
		// the incumbent stops. It must be consumed on every outcome: a leftover
		// file would make the next attempt refuse to publish its own evidence.
		if err := removeAccountHandoffEvidence(); err != nil {
			result = errors.Join(result, err)
		}
	}()
	result = switchDeploymentAccount(ctx, handoff, func(ctx context.Context) error {
		if err := transferDeploymentPaths(ctx, materials, handoff.Lease.Slot); err != nil {
			return err
		}
		// Publish the startup-only instruction first. Neither file takes
		// effect until the following daemon-reload.
		if err := installFile(temporary, adopt, 0644, ""); err != nil {
			return err
		}
		published = true
		if err := installFile(active, steady, 0644, ""); err != nil {
			return err
		}
		if err := command(ctx, "/usr/bin/systemctl", "daemon-reload"); err != nil {
			return err
		}
		// The dedicated deployd inherits its listening socket from systemd, so it
		// must be socket-activated: `serve` exits without an activation
		// descriptor, and starting the service unit alone never supplies one.
		//
		// restart, not start: systemd drops the listening descriptor when a
		// socket unit's file changed under it, leaving a unit that reports
		// active while every connect is refused.
		if err := command(ctx, "/usr/bin/systemctl", "restart", deploydSocketUnit); err != nil {
			return err
		}
		return nil
	})
	return result
}

// removeAccountHandoffEvidence consumes the live handoff evidence this attempt
// published. Leaving it behind makes a later handoff refuse to publish its own
// evidence, so a failed attempt would block every retry.
func removeAccountHandoffEvidence() error {
	info, err := os.Lstat(deployd.AccountHandoffFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, gid, err := dedicatedDeploymentIDs()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || int(stat.Gid) != gid || !info.Mode().IsRegular() || info.Mode().Perm() != 0640 {
		return errors.New("account handoff evidence is not a protected root-owned file")
	}
	if err := os.Remove(deployd.AccountHandoffFile); err != nil {
		return err
	}
	return syncDir(filepath.Dir(deployd.AccountHandoffFile))
}

func transferDeploymentPaths(ctx context.Context, materials, active string) error {
	if err := deployd.VerifyUnprivilegedSlots(ctx); err != nil {
		return err
	}
	cfg := deployd.Defaults()
	slots := filepath.Join(cfg.ReleaseRoot, "slots")
	route, err := deployd.EffectiveActivePath(cfg.ActivePath)
	if err != nil || validateAccountMigrationRoute(route, filepath.Join(slots, active)) != nil {
		return errors.New("active route changed before transferring deployment paths")
	}
	if err := safeParents(slots); err != nil {
		return err
	}
	uid, _, err := dedicatedDeploymentIDs()
	if err != nil {
		return err
	}
	for _, slot := range []string{"blue", "green"} {
		if err := transferLegacySlotDirectories(filepath.Join(slots, slot), filepath.Join("/run/agent-vm-native", slot, "app.sock"), uid); err != nil {
			return fmt.Errorf("transfer %s slot directories: %w", slot, err)
		}
	}
	if err := adoptActiveRoute(cfg.ActivePath, deployd.ManagedRoute, slots); err != nil {
		return err
	}
	return command(ctx, "/usr/bin/systemd-tmpfiles", "--create", "--prefix="+slots,
		filepath.Join(materials, "assets/tmpfiles.d/agent-vm-fresh-accounts.conf"))
}

func prepareHandoffAccount(ctx context.Context, input Inputs) error {
	cfg, err := LoadConfig(input.Config)
	if err != nil {
		return err
	}
	assets := filepath.Join(input.Materials, "assets")
	if err := command(ctx, "/usr/bin/systemd-sysusers", filepath.Join(assets, "sysusers.d/agent-vm-native.conf")); err != nil {
		return err
	}
	expected, err := matchingCandidateConfig(targets["deployd_config"].path, cfg.Files["deployd_config"])
	if err != nil {
		return fmt.Errorf("validate deployment configuration before account handoff: %w", err)
	}
	_, gid, err := dedicatedDeploymentIDs()
	if err != nil {
		return err
	}
	if err := grantCandidateConfigRead(targets["deployd_config"].path, expected, gid); err != nil {
		return fmt.Errorf("grant deployment configuration access: %w", err)
	}
	// The dedicated accounts need their runtime directories recreated at every
	// boot, including the deployd singleton lock inside /run. Keep the rule
	// file installed so systemd-tmpfiles applies it after a host restart.
	freshAccounts := filepath.Join("/etc/tmpfiles.d", "agent-vm-fresh-accounts.conf")
	if err := copyInstall(filepath.Join(assets, "tmpfiles.d/agent-vm-fresh-accounts.conf"), freshAccounts, 0644, ""); err != nil {
		return err
	}
	for _, rule := range []struct{ file, source, prefix string }{
		{"agent-vm-native.conf", filepath.Join(assets, "tmpfiles.d/agent-vm-native.conf"), "/etc/agent-vm-native/backup"},
		{"agent-vm-native.conf", filepath.Join(assets, "tmpfiles.d/agent-vm-native.conf"), "/var/lib/agent-vm-native/routing"},
		{"agent-vm-fresh-accounts.conf", freshAccounts, filepath.Dir(deployd.DedicatedDatabase)},
	} {
		if err := command(ctx, "/usr/bin/systemd-tmpfiles", "--create", "--prefix="+rule.prefix, rule.source); err != nil {
			return err
		}
	}
	// Do not apply the /run prefix here. Its deployd.lock rule hands the file to
	// agent-vm-deploy, while the journal copy that follows requires the incumbent
	// root-owned lock inode. The installed rule file takes effect at the next
	// boot, which is exactly when the dedicated account needs it.
	return installBackupInputs(cfg)
}

func deploymentAccountDropIns(account []byte) (steady, adopt []byte) {
	const command = migrationDeployBinary + " serve --config /etc/agent-vm-native/deployd.json"
	steady = append(bytes.Clone(account), []byte("\n[Service]\nExecStart=\nExecStart="+command+"\n")...)
	adopt = []byte("[Service]\nExecStart=\nExecStart=" + command + " --adopt-account-handoff\n")
	return
}

func removeAccountHandoffDropIn(path string, expected []byte) error {
	if err := safeParents(filepath.Dir(path)); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !rootProtected(info, 0) || !info.Mode().IsRegular() || info.Mode().Perm() != 0644 {
		return errors.New("startup-only unit configuration is not a protected root-owned file")
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, expected) {
		return errors.New("startup-only unit configuration changed")
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// switchDeploymentAccount is the final host handoff, after the new slot is
// serving and the old slot is stopped. The caller holds the release-update
// lock and has prepared the new unit and filesystem permissions. startNew
// publishes the prepared unit and starts it with --adopt-account-handoff.
func switchDeploymentAccount(ctx context.Context, handoff deployd.AccountHandoff, startNew func(context.Context) error) error {
	if os.Geteuid() != 0 || startNew == nil {
		return errors.New("deployment account handoff requires local root and a prepared replacement unit")
	}
	lease := handoff.Lease
	if lease.Validate(time.Now(), lease.Slot) != nil || handoff.StateAuthority == "" {
		return errors.New("deployment account handoff requires a verified live lease")
	}
	uid, gid, err := dedicatedDeploymentIDs()
	if err != nil {
		return err
	}
	owner, err := exec.CommandContext(ctx, "/usr/bin/systemctl", "show", deploymentUnit, "--property=MainPID", "--value").Output()
	pid, pidErr := strconv.Atoi(strings.TrimSpace(string(owner)))
	if err != nil || pidErr != nil || pid != lease.OwnerPID || lease.OwnerStartTime == "" ||
		controllerrole.ProcessStartToken(lease.OwnerPID) != lease.OwnerStartTime || lease.OwnerPID <= 0 {
		return errors.New("active lease is not owned by the incumbent deployment unit")
	}
	cfg := deployd.Defaults()
	lease, err = deployd.WitnessAccountHandoff(ctx, lease.Slot, lease)
	if err != nil {
		return err
	}
	handoff.Lease = lease
	body, err := json.Marshal(handoff)
	if err != nil {
		return err
	}
	if err := installFile(deployd.AccountHandoffFile, body, 0640, "agent-vm-deploy"); err != nil {
		return err
	}
	if !lease.Expiry.After(time.Now().Add(5 * time.Second)) {
		return errors.New("incumbent left insufficient lease time for account handoff")
	}
	bounded, cancel := context.WithDeadline(ctx, lease.Expiry)
	defer cancel()
	if err := command(bounded, "/usr/bin/systemctl", "stop", deploymentUnit); err != nil {
		return fmt.Errorf("incumbent stop outcome requires reconciliation: %w", err)
	}
	if err := copyStoppedDeploymentJournal(bounded, cfg, uid, gid); err != nil {
		// startNew has not been called, so the incumbent unit still points
		// to the original executable and journal.
		restore, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		restoreErr := command(restore, "/usr/bin/systemctl", "start", deploymentUnit)
		return errors.Join(fmt.Errorf("copy deployment journal: %w", err), restoreErr)
	}
	if err := startNew(bounded); err != nil {
		return fmt.Errorf("replacement deployment startup requires reconciliation: %w", err)
	}
	client := deployd.Client{Socket: cfg.Socket, Timeout: time.Second}
	for {
		payload, _, err := client.Call(bounded, "GET", "/v1/status", nil)
		if err == nil && dedicatedHandoffConfirmed(payload, handoff, time.Now()) {
			return nil
		}
		select {
		case <-bounded.Done():
			return errors.New("replacement controller ownership was not confirmed; retain the handoff evidence and journal")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func copyStoppedDeploymentJournal(ctx context.Context, cfg deployd.Config, uid, gid int) error {
	if err := safeParents(filepath.Dir(cfg.LockPath)); err != nil {
		return err
	}
	info, err := os.Lstat(cfg.LockPath)
	if err != nil || !rootProtected(info, 0) || !info.Mode().IsRegular() || info.Sys().(*syscall.Stat_t).Nlink != 1 {
		return errors.New("deployment singleton lock is not a protected root-owned file")
	}
	lock, err := native.AcquireLock(cfg.LockPath, true, true)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := transferRecoveryOwnership(ctx, cfg.Migration.RecoveryDirectory, uid, gid); err != nil {
		return fmt.Errorf("transfer deployment recovery material: %w", err)
	}
	if err := copyAccountJournal(ctx, cfg.Database, deployd.DedicatedDatabase, uid, gid); err != nil {
		return err
	}
	// Reopen the same root-owned lock without following symlinks. Changing
	// the owner keeps the existing lock inode through the handoff.
	fd, err := syscall.Open(cfg.LockPath, syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), cfg.LockPath)
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return errors.New("deployment singleton lock is not a protected root-owned file")
	}
	return errors.Join(file.Chown(uid, gid), file.Chmod(0600), file.Sync())
}

func dedicatedDeploymentIDs() (int, int, error) {
	account, err := user.Lookup("agent-vm-deploy")
	if err != nil {
		return 0, 0, err
	}
	uid, uidErr := strconv.Atoi(account.Uid)
	gid, gidErr := strconv.Atoi(account.Gid)
	if uidErr != nil || gidErr != nil || uid <= 0 || gid <= 0 {
		return 0, 0, errors.New("deployment account has invalid identity")
	}
	return uid, gid, nil
}

func dedicatedHandoffConfirmed(payload []byte, expected deployd.AccountHandoff, now time.Time) bool {
	var envelope struct {
		OK    bool `json:"ok"`
		Value struct {
			Dedicated bool                    `json:"dedicated_service_account"`
			Observed  deployd.RuntimeSnapshot `json:"observation"`
		} `json:"value"`
	}
	if json.Unmarshal(payload, &envelope) != nil || !envelope.OK || !envelope.Value.Dedicated {
		return false
	}
	snapshot := envelope.Value.Observed
	prior := expected.Lease
	active := snapshot.Slots[prior.Slot]
	next := active.ControllerLease
	return snapshot.Consistent && snapshot.FreshUntil.After(now) && snapshot.ActiveSlot == prior.Slot &&
		snapshot.ControllerSlot == prior.Slot && active.ProcessUp && active.Ready && active.TrafficAccepting &&
		active.ControllerOwned && active.StateAuthority == expected.StateAuthority && next != nil &&
		next.Validate(now, prior.Slot) == nil && next.Generation == prior.Generation && next.Term > prior.Term &&
		next.OwnerPID > 0 && next.OwnerPID != prior.OwnerPID && next.OwnerStartTime != ""
}
