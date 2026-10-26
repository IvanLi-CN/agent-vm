package observe

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLinuxCPUObserverVerifiesQuotaAndPressure(t *testing.T) {
	files := map[string]string{
		"/proc/self/mountinfo": "36 25 0:32 / /sys/fs/cgroup rw,nosuid,nodev,noexec,relatime - cgroup2 cgroup rw\n",
		"/proc/self/cgroup":    "0::/system.slice/agent-vm.service\n",
		"/sys/fs/cgroup/system.slice/agent-vm.service/cpu.max":      "max 100000\n",
		"/sys/fs/cgroup/system.slice/cpu.max":                       "150000 100000\n",
		"/sys/fs/cgroup/cpu.max":                                    "750000 100000\n",
		"/sys/fs/cgroup/system.slice/agent-vm.service/cpu.pressure": "some avg10=12.5 avg60=11.0 avg300=9.0 total=1200\nfull avg10=8.5 avg60=7.0 avg300=5.0 total=800\n",
	}
	observer := &LinuxCPUObserver{
		Root: "/sys/fs/cgroup", CgroupFile: "/proc/self/cgroup", MountsFile: "/proc/self/mountinfo",
		ReadFile: func(path string) ([]byte, error) {
			value, ok := files[path]
			if !ok {
				return nil, os.ErrNotExist
			}
			return []byte(value), nil
		},
		CPUCount: func() int { return 16 }, Now: func() time.Time { return time.Unix(0, 0) },
	}
	observation, err := observer.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if observation.Capacity != 1 || observation.Full.Avg10 != 8.5 || observation.CgroupPath != "/system.slice/agent-vm.service" {
		t.Fatalf("observation = %#v", observation)
	}
	if observation.Map()["verified"] != true || !strings.Contains(observation.Metric, "cpu.pressure") {
		t.Fatalf("mapped observation = %#v", observation.Map())
	}
}

func TestLinuxCPUObserverRejectsUnverifiedPressure(t *testing.T) {
	files := map[string]string{
		"/proc/self/mountinfo":        "36 25 0:32 / /sys/fs/cgroup rw - cgroup2 cgroup rw\n",
		"/proc/self/cgroup":           "0::/\n",
		"/sys/fs/cgroup/cpu.max":      "max 100000\n",
		"/sys/fs/cgroup/cpu.pressure": "some avg10=1 avg60=1 avg300=1 total=1\nfull avg10=2 avg60=1 avg300=1 total=1\n",
	}
	observer := &LinuxCPUObserver{ReadFile: func(path string) ([]byte, error) {
		value, ok := files[path]
		if !ok {
			return nil, os.ErrNotExist
		}
		return []byte(value), nil
	}, CPUCount: func() int { return 2 }}
	if _, err := observer.Observe(context.Background()); err == nil || !strings.Contains(err.Error(), "full pressure") {
		t.Fatalf("invalid pressure error = %v", err)
	}
}
