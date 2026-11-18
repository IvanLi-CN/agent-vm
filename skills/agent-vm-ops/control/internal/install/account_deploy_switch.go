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

// handoffDeploymentUnit keeps the incumbent unit definition in place up to
// the point the journal transfer succeeds. The passed materials are from the
// verified, private installation snapshot.
func handoffDeploymentUnit(ctx context.Context, handoff deployd.AccountHandoff, materials string) error {
	account, err := os.ReadFile(filepath.Join(materials, "assets/systemd/fresh-deploy-account.conf"))
	if err != nil {
		return err
	}
	steady, adopt := deploymentAccountDropIns(account)
	const directory = "/etc/systemd/system/" + deploymentUnit + ".d"
	active := filepath.Join(directory, "20-service-account.conf")
	temporary := filepath.Join(directory, "21-account-handoff.conf")
	if err := switchDeploymentAccount(ctx, handoff, func(ctx context.Context) error {
		// Publish the startup-only instruction first. Neither file takes
		// effect until the following daemon-reload.
		if err := installFile(temporary, adopt, 0644, ""); err != nil {
			return err
		}
		if err := installFile(active, steady, 0644, ""); err != nil {
			return err
		}
		if err := command(ctx, "/usr/bin/systemctl", "daemon-reload"); err != nil {
			return err
		}
		return command(ctx, "/usr/bin/systemctl", "start", deploymentUnit)
	}); err != nil {
		return err
	}
	if err := removeAccountHandoffDropIn(temporary, adopt); err != nil {
		return fmt.Errorf("controller adopted; cannot clear startup-only unit configuration: %w", err)
	}
	if err := command(ctx, "/usr/bin/systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("controller adopted; systemd configuration reload failed: %w", err)
	}
	return nil
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
	if err != nil || !rootProtected(info, 0) || !info.Mode().IsRegular() || info.Mode().Perm() != 0644 {
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
	body, err := json.Marshal(handoff)
	if err != nil {
		return err
	}
	if err := installFile(deployd.AccountHandoffFile, body, 0640, "agent-vm-deploy"); err != nil {
		return err
	}
	if err := deployd.WitnessAccountHandoff(ctx, lease.Slot, lease); err != nil {
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
