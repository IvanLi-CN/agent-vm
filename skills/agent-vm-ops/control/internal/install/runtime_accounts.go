package install

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
)

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
