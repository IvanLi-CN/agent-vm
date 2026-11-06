//go:build linux

package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// VerifyRecoveryQuiescence verifies that the systemd authority has no surviving
// child processes and Incus has no background operations. It is deliberately
// unavailable outside the canonical control service.
func (c *IncusClient) VerifyRecoveryQuiescence(ctx context.Context) error {
	if c == nil || c.Runner == nil || WorkerOperationID(ctx) != "" {
		return errors.New("recovery quiescence requires the resident authority")
	}
	current, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return errors.New("control process cgroup is unverified")
	}
	groups := map[string]string{
		"agent-vm-native.service":       "/system.slice/agent-vm-native.service",
		"agent-vm-native@blue.service":  `/system.slice/system-agent\x2dvm\x2dnative.slice/agent-vm-native@blue.service`,
		"agent-vm-native@green.service": `/system.slice/system-agent\x2dvm\x2dnative.slice/agent-vm-native@green.service`,
	}
	ownUnit := ""
	for unit, group := range groups {
		if recoveryCgroupMatches(current, group) {
			ownUnit = unit
		}
	}
	if ownUnit == "" {
		return errors.New("control process cgroup is unverified")
	}
	identities := make(map[string]string)
	verify := func(initial bool) error {
		for _, unit := range []string{"agent-vm-native.service", "agent-vm-native@blue.service", "agent-vm-native@green.service"} {
			result, err := c.Runner.Run(ctx, []string{"systemctl", "show", unit, "--property=MainPID", "--property=ControlGroup", "--property=KillMode", "--property=Delegate"}, nil)
			if err != nil || result.Code != 0 || result.StdoutTruncated || result.StderrTruncated {
				return errors.New("control service identity cannot be verified")
			}
			_, pid, err := recoveryUnitGroup(result.Stdout, groups[unit])
			if err != nil {
				return err
			}
			if unit == ownUnit && pid != os.Getpid() {
				return errors.New("control service owner changed")
			}
			if ownUnit != "agent-vm-native.service" && unit == "agent-vm-native.service" && pid != 0 {
				return errors.New("legacy singleton still owns processes")
			}
			if ownUnit == "agent-vm-native.service" && unit != ownUnit && pid != 0 {
				return errors.New("slot processes still exist during singleton recovery")
			}
			if err := checkRecoveryCgroup(filepath.Join("/sys/fs/cgroup", groups[unit]), pid); err != nil {
				return err
			}
			identity := string(result.Stdout)
			if !initial && identities[unit] != identity {
				return errors.New("control service identity changed during recovery")
			}
			identities[unit] = identity
		}
		return nil
	}
	if err := verify(true); err != nil {
		return err
	}
	result, err := c.Runner.Run(ctx, []string{"incus", "operation", "list", "--all-projects", "--format=json"}, nil)
	if err != nil || result.Code != 0 || result.StdoutTruncated || result.StderrTruncated {
		return errors.New("native background operations cannot be verified")
	}
	if err := recoveryOperationsClear(result.Stdout); err != nil {
		return err
	}
	return verify(false)
}

// recoveryUnitGroup accepts only a fixed systemd unit's protected process
// boundary. An inactive unit must have neither a PID nor a remaining cgroup.
func recoveryUnitGroup(output []byte, expectedGroup string) (string, int, error) {
	properties := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if _, exists := properties[key]; !ok || exists {
			return "", 0, errors.New("invalid control service identity")
		}
		properties[key] = value
	}
	pid, err := strconv.Atoi(properties["MainPID"])
	group := properties["ControlGroup"]
	if len(properties) != 4 || err != nil || pid < 0 || properties["KillMode"] != "control-group" || properties["Delegate"] != "no" || (pid == 0 && group != "") || (pid != 0 && group != expectedGroup) {
		return "", 0, errors.New("control service process boundary is unverified")
	}
	return group, pid, nil
}

func recoveryServiceGroup(output []byte, self int) (string, error) {
	properties := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if _, exists := properties[key]; !ok || exists {
			return "", errors.New("invalid control service identity")
		}
		properties[key] = value
	}
	group := properties["ControlGroup"]
	if len(properties) != 4 || properties["MainPID"] != strconv.Itoa(self) || properties["KillMode"] != "control-group" || properties["Delegate"] != "no" || group != "/system.slice/agent-vm-native.service" {
		return "", errors.New("control service process boundary is unverified")
	}
	return group, nil
}

func recoveryCgroupMatches(output []byte, group string) bool {
	return strings.TrimSpace(string(output)) == "0::"+group
}

func recoveryOperationsClear(output []byte) error {
	var operations []json.RawMessage
	if !strings.HasPrefix(strings.TrimSpace(string(output)), "[") || json.Unmarshal(output, &operations) != nil || len(operations) != 0 {
		return errors.New("native background operations are not clear")
	}
	return nil
}

func checkRecoveryCgroup(directory string, self int) error {
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) && self == 0 {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("control cgroup directory is unverified")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("cannot inspect control cgroup: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return errors.New("control cgroup has an unverified child group")
		}
	}
	raw, err := os.ReadFile(filepath.Join(directory, "cgroup.procs"))
	if err != nil {
		return fmt.Errorf("cannot inspect control processes: %w", err)
	}
	ids := strings.Fields(string(raw))
	if self == 0 && len(ids) == 0 {
		return nil
	}
	if self == 0 || len(ids) != 1 || ids[0] != strconv.Itoa(self) {
		return errors.New("control cgroup contains other processes")
	}
	return nil
}
