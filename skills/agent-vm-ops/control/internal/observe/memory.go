package observe

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

const (
	defaultMemoryCgroupRoot = "/sys/fs/cgroup"
	defaultMemoryInfoFile   = "/proc/meminfo"
	defaultMemoryMountsFile = "/proc/self/mountinfo"
	gibibyte                = int64(1024 * 1024 * 1024)
)

// MemoryObservation is a verified observation of the visible CT root
// cgroup. It is deliberately separate from logical admission accounting.
type MemoryObservation struct {
	Scope          string
	Sources        []string
	LimitBytes     int64
	AvailableBytes int64
	CurrentBytes   *int64
	ThresholdBytes int64
	OOMEvents      map[string]int64
}

func (observation MemoryObservation) Map() map[string]any {
	var current any
	if observation.CurrentBytes != nil {
		current = *observation.CurrentBytes
	}
	var oom any
	if len(observation.OOMEvents) > 0 {
		oom = observation.OOMEvents
	}
	return map[string]any{
		"scope": observation.Scope, "sources": observation.Sources,
		"limit_bytes": observation.LimitBytes, "available_bytes": observation.AvailableBytes,
		"current_bytes": current, "threshold_bytes": observation.ThresholdBytes,
		"oom_events": oom, "verified": true,
	}
}

// LinuxMemoryObserver reads the visible CT root cgroup and, when the cgroup
// limit is unlimited, a verified LXCfs /proc/meminfo view. The seams are
// injectable so contract tests do not need a Linux host.
type LinuxMemoryObserver struct {
	Root        string
	MeminfoFile string
	MountsFile  string
	ReadFile    func(string) ([]byte, error)
}

func NewMemoryObserver() *LinuxMemoryObserver {
	return &LinuxMemoryObserver{
		Root:        defaultMemoryCgroupRoot,
		MeminfoFile: defaultMemoryInfoFile,
		MountsFile:  defaultMemoryMountsFile,
		ReadFile:    os.ReadFile,
	}
}

func (observer *LinuxMemoryObserver) Observe(ctx context.Context) (MemoryObservation, error) {
	if observer == nil {
		return MemoryObservation{}, errors.New("memory observer is not configured")
	}
	if err := ctx.Err(); err != nil {
		return MemoryObservation{}, err
	}
	root := observer.Root
	if root == "" {
		root = defaultMemoryCgroupRoot
	}
	rawLimitBytes, err := observer.read(filepathJoin(root, "memory.max"))
	if err != nil {
		return MemoryObservation{}, fmt.Errorf("read memory.max: %w", err)
	}
	rawLimit := strings.TrimSpace(string(rawLimitBytes))
	lxcfs, lxcfsErr := observer.lxcfsMemory()
	if lxcfsErr != nil {
		return MemoryObservation{}, lxcfsErr
	}
	sources := []string{"cgroup-v2"}
	var limit, available int64
	var current *int64
	if rawLimit == "max" {
		if lxcfs == nil {
			return MemoryObservation{}, errors.New("unlimited CT cgroup requires a verified LXCfs memory view")
		}
		limit, available = lxcfs.total, lxcfs.available
		if rawCurrent, currentErr := observer.read(filepathJoin(root, "memory.current")); currentErr == nil {
			value, parseErr := parseUnsigned(string(rawCurrent), "memory.current")
			if parseErr != nil {
				return MemoryObservation{}, parseErr
			}
			if value > limit {
				return MemoryObservation{}, errors.New("CT memory accounting exceeds its LXCfs capacity")
			}
			current = &value
		}
	} else {
		value, parseErr := parseUnsigned(rawLimit, "memory.max")
		if parseErr != nil || value <= 0 {
			if parseErr != nil {
				return MemoryObservation{}, parseErr
			}
			return MemoryObservation{}, errors.New("memory.max must be positive")
		}
		limit = value
		rawCurrent, currentErr := observer.read(filepathJoin(root, "memory.current"))
		if currentErr != nil {
			return MemoryObservation{}, fmt.Errorf("read memory.current: %w", currentErr)
		}
		value, parseErr = parseUnsigned(string(rawCurrent), "memory.current")
		if parseErr != nil {
			return MemoryObservation{}, parseErr
		}
		if value > limit {
			return MemoryObservation{}, errors.New("CT memory accounting exceeds its cgroup limit")
		}
		current = &value
		inactive, statErr := observer.inactiveFile(filepathJoin(root, "memory.stat"), value)
		if statErr != nil {
			return MemoryObservation{}, statErr
		}
		headroom := limit - value
		if inactive > math.MaxInt64-headroom {
			headroom = math.MaxInt64
		} else {
			headroom += inactive
		}
		available = minInt64(limit, headroom)
	}
	if lxcfs != nil {
		sources = append(sources, "lxcfs")
		limit = minInt64(limit, lxcfs.total)
		if current != nil && *current > limit {
			return MemoryObservation{}, errors.New("CT memory accounting exceeds its effective capacity")
		}
		available = minInt64(minInt64(available, lxcfs.available), limit)
	}
	threshold := limit / 10
	if limit%10 != 0 {
		threshold++
	}
	if threshold < gibibyte {
		threshold = gibibyte
	}
	return MemoryObservation{
		Scope:          "ct-memory",
		Sources:        sources,
		LimitBytes:     limit,
		AvailableBytes: available,
		CurrentBytes:   current,
		ThresholdBytes: threshold,
		OOMEvents:      observer.oomEvents(filepathJoin(root, "memory.events")),
	}, nil
}

type lxcfsMemory struct {
	total     int64
	available int64
}

func (observer *LinuxMemoryObserver) lxcfsMemory() (*lxcfsMemory, error) {
	mounts, err := observer.read(observer.mountsPath())
	if err != nil {
		return nil, nil
	}
	found := false
	for _, line := range strings.Split(string(mounts), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 10 {
			separator := 0
			for index, field := range fields {
				if field == "-" {
					separator = index
					break
				}
			}
			if separator > 5 && separator+2 < len(fields) && fields[4] == "/proc/meminfo" && fields[separator+1] == "fuse.lxcfs" {
				found = true
				break
			}
		}
	}
	if !found {
		return nil, nil
	}
	raw, err := observer.read(observer.meminfoPath())
	if err != nil {
		return nil, fmt.Errorf("read LXCfs memory view: %w", err)
	}
	values := map[string]int64{}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 3 || (fields[0] != "MemTotal:" && fields[0] != "MemAvailable:") || fields[2] != "kB" {
			continue
		}
		value, parseErr := parseUnsigned(fields[1], fields[0])
		if parseErr != nil || value > math.MaxInt64/1024 {
			return nil, errors.New("LXCfs memory metrics are invalid")
		}
		values[strings.TrimSuffix(fields[0], ":")] = value * 1024
	}
	total, totalOK := values["MemTotal"]
	available, availableOK := values["MemAvailable"]
	if !totalOK || !availableOK || total <= 0 || available < 0 || available > total {
		return nil, errors.New("LXCfs memory metrics are missing or inconsistent")
	}
	return &lxcfsMemory{total: total, available: available}, nil
}

func (observer *LinuxMemoryObserver) inactiveFile(path string, current int64) (int64, error) {
	raw, err := observer.read(path)
	if err != nil {
		return 0, fmt.Errorf("read memory.stat: %w", err)
	}
	count := 0
	var value int64
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 || fields[0] != "inactive_file" {
			continue
		}
		count++
		parsed, parseErr := parseUnsigned(fields[1], "inactive_file")
		if parseErr != nil {
			return 0, parseErr
		}
		value = parsed
	}
	if count != 1 {
		return 0, errors.New("inactive_file memory metric is missing or duplicated")
	}
	if value > current {
		return 0, errors.New("CT memory accounting is inconsistent")
	}
	return value, nil
}

func (observer *LinuxMemoryObserver) oomEvents(path string) map[string]int64 {
	raw, err := observer.read(path)
	if err != nil {
		return nil
	}
	result := map[string]int64{}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 || (fields[0] != "oom" && fields[0] != "oom_kill" && fields[0] != "oom_group_kill") {
			continue
		}
		if _, exists := result[fields[0]]; exists {
			return nil
		}
		value, parseErr := parseUnsigned(fields[1], "memory.events."+fields[0])
		if parseErr != nil {
			return nil
		}
		result[fields[0]] = value
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func (observer *LinuxMemoryObserver) read(path string) ([]byte, error) {
	readFile := observer.ReadFile
	if readFile == nil {
		readFile = os.ReadFile
	}
	return readFile(path)
}

func (observer *LinuxMemoryObserver) mountsPath() string {
	if observer.MountsFile != "" {
		return observer.MountsFile
	}
	return defaultMemoryMountsFile
}

func (observer *LinuxMemoryObserver) meminfoPath() string {
	if observer.MeminfoFile != "" {
		return observer.MeminfoFile
	}
	return defaultMemoryInfoFile
}

func parseUnsigned(raw, name string) (int64, error) {
	value := strings.TrimSpace(raw)
	if value == "" || strings.HasPrefix(value, "-") {
		return 0, fmt.Errorf("%s must be a non-negative integer", name)
	}
	parsed, err := strconv.ParseUint(value, 10, 63)
	if err != nil {
		return 0, fmt.Errorf("%s is invalid: %w", name, err)
	}
	return int64(parsed), nil
}

func minInt64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
}

func filepathJoin(root, name string) string {
	if root == "/" {
		return "/" + name
	}
	return strings.TrimSuffix(root, "/") + "/" + name
}
