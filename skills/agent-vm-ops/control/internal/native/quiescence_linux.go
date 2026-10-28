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
	result, err := c.Runner.Run(ctx, []string{"systemctl", "show", "agent-vm-native.service", "--property=MainPID", "--property=ControlGroup", "--property=KillMode", "--property=Delegate"}, nil)
	if err != nil || result.Code != 0 || result.StdoutTruncated || result.StderrTruncated {
		return errors.New("control service identity cannot be verified")
	}
	properties := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(result.Stdout)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if _, exists := properties[key]; !ok || exists {
			return errors.New("invalid control service identity")
		}
		properties[key] = value
	}
	group := properties["ControlGroup"]
	if len(properties) != 4 || properties["MainPID"] != strconv.Itoa(os.Getpid()) || properties["KillMode"] != "control-group" || properties["Delegate"] != "no" || group != "/system.slice/agent-vm-native.service" {
		return errors.New("control service process boundary is unverified")
	}
	current, err := os.ReadFile("/proc/self/cgroup")
	if err != nil || strings.TrimSpace(string(current)) != "0::"+group {
		return errors.New("control process cgroup is unverified")
	}
	if err := checkRecoveryCgroup(filepath.Join("/sys/fs/cgroup", group), os.Getpid()); err != nil {
		return err
	}
	result, err = c.Runner.Run(ctx, []string{"incus", "operation", "list", "--all-projects", "--format=json"}, nil)
	if err != nil || result.Code != 0 || result.StdoutTruncated || result.StderrTruncated {
		return errors.New("native background operations cannot be verified")
	}
	var operations []json.RawMessage
	if !strings.HasPrefix(strings.TrimSpace(string(result.Stdout)), "[") || json.Unmarshal(result.Stdout, &operations) != nil || len(operations) != 0 {
		return errors.New("native background operations are not clear")
	}
	return checkRecoveryCgroup(filepath.Join("/sys/fs/cgroup", group), os.Getpid())
}

func checkRecoveryCgroup(directory string, self int) error {
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
	if len(ids) != 1 || ids[0] != strconv.Itoa(self) {
		return errors.New("control cgroup contains other processes")
	}
	return nil
}
