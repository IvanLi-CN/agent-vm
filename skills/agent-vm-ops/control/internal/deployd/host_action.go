package deployd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const hostActionSocket = "/run/agent-vm-deploy-host/action.sock"
const slotUser = "agent-vm-server"
const deployUser = "agent-vm-deploy"
const ServerAuthorityCA = "/etc/agent-vm-native/tls/server-authority.crt"

type hostAction struct {
	Action   string `json:"action"`
	Slot     string `json:"slot"`
	Layout   string `json:"layout,omitempty"`
	Rollback bool   `json:"rollback,omitempty"`
}

func (action hostAction) validate() error {
	if err := validSlot(action.Slot); err != nil {
		return err
	}
	switch action.Action {
	case "start", "restart", "restore-restart-policy":
		if action.Layout != "server" && action.Layout != "control" {
			return errors.New("invalid slot binary layout")
		}
		if action.Action == "restore-restart-policy" && action.Rollback {
			return errors.New("restart policy restoration requires the normal policy")
		}
	case "stop", "enable", "reset-failed":
		if action.Layout != "" || action.Rollback {
			return errors.New("unexpected host action properties")
		}
	case "initialize", "resume-initialize":
		if action.Slot != "blue" || action.Layout != "" || action.Rollback {
			return errors.New("initialization requires the fixed blue slot")
		}
	default:
		return errors.New("invalid host action")
	}
	return nil
}

func isDeployAccount(uid int) bool {
	return isServiceAccount(uid, deployUser)
}

func IsServerAccount(uid int) bool { return isServiceAccount(uid, slotUser) }

func isServiceAccount(uid int, name string) bool {
	if uid == 0 {
		return false
	}
	account, err := user.Lookup(name)
	return err == nil && account.Uid == strconv.Itoa(uid)
}

// Staging can replace binaries by renaming slot directories. These paths must
// no longer be used by a root slot when writable by the deployment account.
func verifyUnprivilegedSlots(ctx context.Context) error {
	for _, slot := range []string{"blue", "green"} {
		if err := verifySlotAccount(ctx, slot, false); err != nil {
			return err
		}
	}
	return nil
}

// VerifyRunningSlotAccount checks systemd's active process, not just the unit.
func VerifyRunningSlotAccount(ctx context.Context, slot string) error {
	return verifySlotAccount(ctx, slot, true)
}

func verifySlotAccount(ctx context.Context, slot string, requireRunning bool) error {
	if err := validSlot(slot); err != nil {
		return err
	}
	account, err := user.Lookup(slotUser)
	if err != nil || account.Uid == "0" {
		return errors.New("slot account missing; complete host permission maintenance before staging")
	}
	unit := "agent-vm-native@" + slot + ".service"
	identity := func() ([]byte, error) {
		return exec.CommandContext(ctx, "/usr/bin/systemctl", "show", unit, "--property=User", "--property=MainPID").Output()
	}
	output, err := identity()
	if err != nil {
		return fmt.Errorf("cannot verify systemd identity of slot %s", slot)
	}
	pid, err := unprivilegedSlotPID(string(output))
	if err != nil {
		return fmt.Errorf("slot %s: %w", slot, err)
	}
	if pid == 0 && requireRunning {
		return fmt.Errorf("slot %s has no running systemd main process", slot)
	}
	if pid != 0 {
		status, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
		if err != nil || !processUsesSlotUID(string(status), account.Uid) {
			return fmt.Errorf("slot %s process identity is unverified; complete host permission maintenance before staging", slot)
		}
		current, err := identity()
		if err != nil || string(current) != string(output) {
			return fmt.Errorf("slot %s main process changed during identity verification", slot)
		}
	}
	return nil
}

// VerifyUnprivilegedSlots guards host account migration before it transfers
// ownership of the directories from which systemd launches the slot binaries.
func VerifyUnprivilegedSlots(ctx context.Context) error {
	return verifyUnprivilegedSlots(ctx)
}

func unprivilegedSlotPID(output string) (int, error) {
	values := make(map[string]string, 2)
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if _, exists := values[key]; !ok || exists {
			return 0, errors.New("invalid systemd slot identity")
		}
		values[key] = value
	}
	pid, err := strconv.Atoi(values["MainPID"])
	if len(values) != 2 || values["User"] != slotUser || err != nil || pid < 0 {
		return 0, errors.New("slot must use the dedicated service account; complete host permission maintenance before staging")
	}
	return pid, nil
}

func processUsesSlotUID(status, uid string) bool {
	for _, line := range strings.Split(status, "\n") {
		if value, found := strings.CutPrefix(line, "Uid:"); found {
			ids := strings.Fields(value)
			return len(ids) == 4 && ids[0] == uid && ids[1] == uid && ids[2] == uid && ids[3] == uid
		}
	}
	return false
}

// RunHostAction handles one connected Unix socket provided by systemd.
func RunHostAction(input *os.File) error {
	if os.Geteuid() != 0 {
		return errors.New("host actions require the systemd root service")
	}
	conn, err := net.FileConn(input)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, ok := conn.(*net.UnixConn); !ok {
		return errors.New("host actions require a Unix connection")
	}
	peer, err := peerUID(conn)
	if err != nil || !isDeployAccount(peer) {
		return errors.New("unauthorized host action peer")
	}
	if err := conn.SetDeadline(time.Now().Add(65 * time.Second)); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	err = handleHostAction(ctx, conn, applyHostAction)
	if _, writeErr := fmt.Fprintln(conn, map[bool]string{true: "ok", false: "rejected"}[err == nil]); writeErr != nil {
		return writeErr
	}
	return err
}

func handleHostAction(ctx context.Context, input io.Reader, apply func(context.Context, hostAction) error) error {
	raw, err := io.ReadAll(io.LimitReader(input, 513))
	if err != nil || len(raw) > 512 {
		return errors.New("host action request exceeds limit")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var action hostAction
	if err := decoder.Decode(&action); err != nil {
		return errors.New("invalid host action request")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("unexpected trailing host action data")
	}
	if err := action.validate(); err != nil {
		return err
	}
	return apply(ctx, action)
}

func dispatchHostAction(ctx context.Context, action hostAction) error {
	if err := action.validate(); err != nil {
		return err
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", hostActionSocket)
	if err != nil {
		return fmt.Errorf("connect to deployment host action socket: %w", err)
	}
	defer conn.Close()
	deadline := time.Now().Add(65 * time.Second)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if err := json.NewEncoder(conn).Encode(action); err != nil {
		return err
	}
	if err := conn.(*net.UnixConn).CloseWrite(); err != nil {
		return err
	}
	reply, err := io.ReadAll(io.LimitReader(conn, 32))
	if err != nil || string(reply) != "ok\n" {
		return errors.New("deployment host action failed")
	}
	return nil
}

func applyHostAction(ctx context.Context, action hostAction) error {
	if err := action.validate(); err != nil {
		return err
	}
	if action.Action == "initialize" || action.Action == "resume-initialize" {
		args := []string{"--quiet", "--wait", "--collect", "--unit=agent-vm-native-initialize.service", "--service-type=oneshot",
			"--uid=" + slotUser, "--gid=agent-vm-web", "--property=SupplementaryGroups=" + slotUser,
			"--property=NoNewPrivileges=yes", "--property=ProtectSystem=strict", "--property=ProtectHome=yes", "--property=PrivateTmp=yes",
			"--property=TimeoutStartSec=50s",
			"--setenv=SSL_CERT_FILE=" + ServerAuthorityCA,
			"/var/lib/agent-vm-native/slots/blue/bin/agent-vm-server", "initialize-state"}
		if action.Action == "resume-initialize" {
			args = append(args, "--resume")
		}
		return exec.CommandContext(ctx, "/usr/bin/systemd-run", args...).Run()
	}
	unit := "agent-vm-native@" + action.Slot + ".service"
	run := func(args ...string) error {
		return exec.CommandContext(ctx, "/usr/bin/systemctl", args...).Run()
	}
	if action.Action == "start" || action.Action == "restart" || action.Action == "restore-restart-policy" {
		dir := filepath.Join("/etc/systemd/system", unit+".d")
		if err := checkHostUnitDirectory(dir); err != nil {
			return err
		}
		name := "agent-vm-server"
		if action.Layout == "control" {
			name = "agent-vm-control"
		}
		dirname := "/var/lib/agent-vm-native/slots/" + action.Slot
		cfg := "/etc/agent-vm-native/slots/" + action.Slot + "/config.json"
		binary := dirname + "/bin/" + name
		restart := "on-failure"
		if action.Rollback {
			restart = "no"
		}
		contents := "[Unit]\nConditionPathExists=\nConditionPathExists=" + binary +
			"\nConditionPathExists=" + cfg + "\n[Service]\nUser=" + slotUser +
			"\nSupplementaryGroups=agent-vm-server agent-vm-rqlite\nEnvironment=INCUS_CONF=/etc/agent-vm-native/incus" +
			"\nEnvironment=SSL_CERT_FILE=" + ServerAuthorityCA +
			"\nEnvironment=AGENT_VM_LEGACY_SLOT=" + action.Slot +
			"\nEnvironment=PATH=/opt/agent-vm-native/compat-incus:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" +
			"\nExecStart=\nExecStart=" + binary + " serve --config " + cfg +
			" --manifest " + dirname + "/release-manifest.json\nRestart=" + restart + "\n"
		if err := writeHostUnit(dir, contents); err != nil {
			return err
		}
		if err := run("daemon-reload"); err != nil {
			return err
		}
		if err := requireHostSlotProperty(ctx, unit, "Restart", restart); err != nil {
			return err
		}
		if action.Action == "restore-restart-policy" {
			return requireHostSlotProperty(ctx, unit, "User", slotUser)
		}
	}
	if action.Action == "start" || action.Action == "restart" || action.Action == "enable" {
		if err := requireHostSlotProperty(ctx, unit, "User", slotUser); err != nil {
			return err
		}
	}
	return run(action.Action, unit)
}

func requireHostSlotProperty(ctx context.Context, unit, key, want string) error {
	current, err := exec.CommandContext(ctx, "/usr/bin/systemctl", "show", unit, "--property="+key, "--value").Output()
	if err != nil || strings.TrimSpace(string(current)) != want {
		return fmt.Errorf("systemd did not load required slot %s", key)
	}
	return nil
}

func checkHostUnitDirectory(dir string) error {
	for {
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !ok || owner.Uid != 0 || !info.IsDir() || info.Mode().Perm()&0022 != 0 {
			return errors.New("untrusted deployment unit directory")
		}
		if dir == "/" {
			return nil
		}
		dir = filepath.Dir(dir)
	}
}

func writeHostUnit(dir, contents string) error {
	file, err := os.CreateTemp(dir, ".release-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := io.WriteString(file, contents)
	err = errors.Join(writeErr, file.Chmod(0644), file.Sync(), file.Close())
	if err != nil {
		return err
	}
	return os.Rename(file.Name(), filepath.Join(dir, "10-release.conf"))
}
