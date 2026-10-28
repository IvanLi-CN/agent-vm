package observe

import (
	"context"
	"strings"
	"testing"
)

func TestLinuxMemoryObserverReadsFiniteCgroupCapacity(t *testing.T) {
	const gib = int64(1024 * 1024 * 1024)
	values := map[string]string{
		"/sys/fs/cgroup/memory.max":     "4294967296\n",
		"/sys/fs/cgroup/memory.current": "3221225472\n",
		"/sys/fs/cgroup/memory.stat":    "anon 1\ninactive_file 0\n",
		"/sys/fs/cgroup/memory.events":  "oom 2\noom_kill 1\n",
		"/proc/self/mountinfo":          "",
	}
	observer := &LinuxMemoryObserver{ReadFile: func(path string) ([]byte, error) {
		value, ok := values[path]
		if !ok {
			return nil, errMissingMemoryMetric{}
		}
		return []byte(value), nil
	}}
	observation, err := observer.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if observation.LimitBytes != 4*gib || observation.AvailableBytes != gib || observation.CurrentBytes == nil || *observation.CurrentBytes != 3*gib {
		t.Fatalf("observation = %#v", observation)
	}
	if observation.ThresholdBytes != gib || observation.OOMEvents["oom_kill"] != 1 {
		t.Fatalf("observation details = %#v", observation)
	}
}

func TestLinuxMemoryObserverUsesVerifiedLXCFSForUnlimitedCgroup(t *testing.T) {
	const gib = int64(1024 * 1024 * 1024)
	values := map[string]string{
		"/sys/fs/cgroup/memory.max":     "max\n",
		"/sys/fs/cgroup/memory.current": "3221225472\n",
		"/proc/self/mountinfo":          "12 1 0:9 /meminfo /proc/meminfo rw - fuse.lxcfs lxcfs rw\n",
		"/proc/meminfo":                 "MemTotal:       4194304 kB\nMemAvailable:   2097152 kB\n",
	}
	observer := &LinuxMemoryObserver{ReadFile: func(path string) ([]byte, error) {
		value, ok := values[path]
		if !ok {
			return nil, errMissingMemoryMetric{}
		}
		return []byte(value), nil
	}}
	observation, err := observer.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if observation.LimitBytes != 4*gib || observation.AvailableBytes != 2*gib || observation.CurrentBytes == nil {
		t.Fatalf("LXCfs observation = %#v", observation)
	}
	if strings.Join(observation.Sources, ",") != "cgroup-v2,lxcfs" {
		t.Fatalf("observation sources = %#v", observation.Sources)
	}
}

func TestLinuxMemoryObserverRejectsContradictoryCurrent(t *testing.T) {
	values := map[string]string{
		"/sys/fs/cgroup/memory.max":     "10\n",
		"/sys/fs/cgroup/memory.current": "11\n",
	}
	observer := &LinuxMemoryObserver{ReadFile: func(path string) ([]byte, error) {
		value, ok := values[path]
		if !ok {
			return nil, errMissingMemoryMetric{}
		}
		return []byte(value), nil
	}}
	if _, err := observer.Observe(context.Background()); err == nil {
		t.Fatal("contradictory memory observation unexpectedly succeeded")
	}
}

type errMissingMemoryMetric struct{}

func (errMissingMemoryMetric) Error() string { return "missing memory metric" }
