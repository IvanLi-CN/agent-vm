package observe

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	defaultCgroupRoot = "/sys/fs/cgroup"
	defaultCgroupFile = "/proc/self/cgroup"
	defaultMountsFile = "/proc/self/mountinfo"
)

type PSIValues struct {
	Avg10  float64 `json:"avg10"`
	Avg60  float64 `json:"avg60"`
	Avg300 float64 `json:"avg300"`
	Total  float64 `json:"total"`
}

type CPUObservation struct {
	Scope      string
	Sources    []string
	Mountpoint string
	MountRoot  string
	Filesystem string
	CgroupPath string
	Metric     string
	Capacity   int64
	Some       PSIValues
	Full       PSIValues
	ObservedAt time.Time
}

func (observation CPUObservation) Pressure() map[string]any {
	return map[string]any{
		"some": map[string]any{
			"avg10": observation.Some.Avg10, "avg60": observation.Some.Avg60,
			"avg300": observation.Some.Avg300, "total": observation.Some.Total,
		},
		"full": map[string]any{
			"avg10": observation.Full.Avg10, "avg60": observation.Full.Avg60,
			"avg300": observation.Full.Avg300, "total": observation.Full.Total,
		},
	}
}

func (observation CPUObservation) Map() map[string]any {
	return map[string]any{
		"scope": observation.Scope, "sources": observation.Sources,
		"mountpoint": observation.Mountpoint, "mount_root": observation.MountRoot,
		"filesystem": observation.Filesystem, "cgroup_path": observation.CgroupPath,
		"metric": observation.Metric, "capacity_vcpu": observation.Capacity,
		"observed_at": observation.ObservedAt.UTC().Format(time.RFC3339Nano),
		"verified":    true, "some": observation.Pressure()["some"], "full": observation.Pressure()["full"],
	}
}

// LinuxCPUObserver reads only the current cgroup's cgroup-v2 metrics. The
// seams are injectable so contract tests do not need a Linux host.
type LinuxCPUObserver struct {
	Root       string
	CgroupFile string
	MountsFile string
	ReadFile   func(string) ([]byte, error)
	CPUCount   func() int
	Now        func() time.Time
}

func NewCPUObserver() *LinuxCPUObserver {
	return &LinuxCPUObserver{
		Root:       defaultCgroupRoot,
		CgroupFile: defaultCgroupFile,
		MountsFile: defaultMountsFile,
		ReadFile:   os.ReadFile,
		CPUCount:   runtime.NumCPU,
		Now:        time.Now,
	}
}

func (observer *LinuxCPUObserver) Observe(ctx context.Context) (CPUObservation, error) {
	if observer == nil {
		return CPUObservation{}, errors.New("CPU observer is not configured")
	}
	if err := ctx.Err(); err != nil {
		return CPUObservation{}, err
	}
	root := observer.Root
	if root == "" {
		root = defaultCgroupRoot
	}
	if err := observer.verifyMount(root); err != nil {
		return CPUObservation{}, err
	}
	cgroupPath, err := observer.currentCgroupPath()
	if err != nil {
		return CPUObservation{}, err
	}
	capacity, err := observer.cpuCapacity(root, cgroupPath)
	if err != nil {
		return CPUObservation{}, err
	}
	metric := cgroupMetricPath(root, cgroupPath, "cpu.pressure")
	pressure, err := observer.readPressure(metric)
	if err != nil {
		return CPUObservation{}, fmt.Errorf("read %s: %w", metric, err)
	}
	now := time.Now
	if observer.Now != nil {
		now = observer.Now
	}
	return CPUObservation{
		Scope: "ct-cpu-pressure", Sources: []string{"cgroupfs"},
		Mountpoint: root, MountRoot: "/", Filesystem: "cgroup2",
		CgroupPath: cgroupPath, Metric: metric, Capacity: capacity,
		Some: pressure.Some, Full: pressure.Full, ObservedAt: now().UTC(),
	}, nil
}

func (observer *LinuxCPUObserver) read(path string) ([]byte, error) {
	readFile := observer.ReadFile
	if readFile == nil {
		readFile = os.ReadFile
	}
	return readFile(path)
}

func (observer *LinuxCPUObserver) verifyMount(root string) error {
	mountsFile := observer.MountsFile
	if mountsFile == "" {
		mountsFile = defaultMountsFile
	}
	raw, err := observer.read(mountsFile)
	if err != nil {
		return fmt.Errorf("read cgroup mounts: %w", err)
	}
	for _, line := range splitLines(string(raw)) {
		parts := strings.SplitN(line, " - ", 2)
		if len(parts) != 2 {
			continue
		}
		left, right := strings.Fields(parts[0]), strings.Fields(parts[1])
		if len(left) >= 5 && len(right) >= 1 && left[3] == "/" && left[4] == root && right[0] == "cgroup2" {
			return nil
		}
	}
	return fmt.Errorf("cgroup2 mount is not verified at %s", root)
}

func (observer *LinuxCPUObserver) currentCgroupPath() (string, error) {
	cgroupFile := observer.CgroupFile
	if cgroupFile == "" {
		cgroupFile = defaultCgroupFile
	}
	raw, err := observer.read(cgroupFile)
	if err != nil {
		return "", fmt.Errorf("read current cgroup: %w", err)
	}
	for _, line := range splitLines(string(raw)) {
		if strings.HasPrefix(line, "0::") {
			path := strings.TrimSpace(strings.TrimPrefix(line, "0::"))
			if validCgroupPath(path) {
				return path, nil
			}
			return "", errors.New("current cgroup path is invalid")
		}
	}
	return "", errors.New("unified cgroup path is missing")
}

func (observer *LinuxCPUObserver) cpuCapacity(root, cgroupPath string) (int64, error) {
	count := runtime.NumCPU
	if observer.CPUCount != nil {
		count = observer.CPUCount
	}
	capacity := int64(count())
	if capacity <= 0 {
		return 0, errors.New("CPU affinity capacity is invalid")
	}
	for _, path := range cgroupAncestors(cgroupPath) {
		raw, err := observer.read(cgroupMetricPath(root, path, "cpu.max"))
		if err != nil {
			return 0, fmt.Errorf("read CPU quota for %s: %w", path, err)
		}
		quota, period, limited, err := parseCPUQuota(string(raw))
		if err != nil {
			return 0, err
		}
		if limited && quota/period < capacity {
			capacity = quota / period
		}
	}
	if capacity <= 0 {
		return 0, errors.New("CT CPU capacity is not positive")
	}
	return capacity, nil
}

func parseCPUQuota(raw string) (quota, period int64, limited bool, err error) {
	fields := strings.Fields(raw)
	if len(fields) != 2 {
		return 0, 0, false, errors.New("CPU quota is malformed")
	}
	period, err = parsePositiveInt(fields[1], "CPU quota period")
	if err != nil {
		return 0, 0, false, err
	}
	if fields[0] == "max" {
		return 0, period, false, nil
	}
	quota, err = parsePositiveInt(fields[0], "CPU quota")
	if err != nil {
		return 0, 0, false, err
	}
	return quota, period, true, nil
}

func parsePositiveInt(raw, name string) (int64, error) {
	if raw == "" || strings.HasPrefix(raw, "+") || strings.HasPrefix(raw, "-") {
		return 0, fmt.Errorf("%s is invalid", name)
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s is invalid", name)
	}
	return value, nil
}

func (observer *LinuxCPUObserver) readPressure(metric string) (struct{ Some, Full PSIValues }, error) {
	raw, err := observer.read(metric)
	if err != nil {
		return struct{ Some, Full PSIValues }{}, err
	}
	values := map[string]PSIValues{}
	for _, line := range splitLines(string(raw)) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] != "some" && fields[0] != "full" {
			return struct{ Some, Full PSIValues }{}, errors.New("CPU PSI contains an unknown scope")
		}
		if _, exists := values[fields[0]]; exists {
			return struct{ Some, Full PSIValues }{}, errors.New("CPU PSI contains a duplicate scope")
		}
		parsed := map[string]float64{}
		for _, field := range fields[1:] {
			parts := strings.SplitN(field, "=", 2)
			if len(parts) != 2 {
				return struct{ Some, Full PSIValues }{}, errors.New("CPU PSI field is malformed")
			}
			if _, exists := parsed[parts[0]]; exists {
				return struct{ Some, Full PSIValues }{}, errors.New("CPU PSI field is duplicated")
			}
			if parts[0] != "avg10" && parts[0] != "avg60" && parts[0] != "avg300" && parts[0] != "total" {
				return struct{ Some, Full PSIValues }{}, errors.New("CPU PSI contains an unknown field")
			}
			if parts[0] == "total" && (strings.ContainsAny(parts[1], ".eE+-") || parts[1] == "") {
				return struct{ Some, Full PSIValues }{}, errors.New("CPU PSI total is invalid")
			}
			number, parseErr := strconv.ParseFloat(parts[1], 64)
			if parseErr != nil || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 || (parts[0] != "total" && number > 100) {
				return struct{ Some, Full PSIValues }{}, errors.New("CPU PSI value is invalid")
			}
			parsed[parts[0]] = number
		}
		if len(parsed) != 4 || !hasPSIFields(parsed) {
			return struct{ Some, Full PSIValues }{}, errors.New("CPU PSI fields are incomplete")
		}
		values[fields[0]] = PSIValues{Avg10: parsed["avg10"], Avg60: parsed["avg60"], Avg300: parsed["avg300"], Total: parsed["total"]}
	}
	some, someOK := values["some"]
	full, fullOK := values["full"]
	if !someOK || !fullOK {
		return struct{ Some, Full PSIValues }{}, errors.New("CPU PSI scopes are incomplete")
	}
	if full.Avg10 > some.Avg10 || full.Avg60 > some.Avg60 || full.Avg300 > some.Avg300 || full.Total > some.Total {
		return struct{ Some, Full PSIValues }{}, errors.New("CPU PSI full pressure exceeds some pressure")
	}
	return struct{ Some, Full PSIValues }{Some: some, Full: full}, nil
}

func hasPSIFields(values map[string]float64) bool {
	for _, name := range []string{"avg10", "avg60", "avg300", "total"} {
		if _, ok := values[name]; !ok {
			return false
		}
	}
	return true
}

func cgroupAncestors(path string) []string {
	result := []string{}
	for current := path; ; {
		result = append(result, current)
		if current == "/" {
			return result
		}
		index := strings.LastIndexByte(current, '/')
		if index <= 0 {
			current = "/"
		} else {
			current = current[:index]
		}
	}
}

func cgroupMetricPath(root, cgroupPath, metric string) string {
	if cgroupPath == "/" {
		return root + "/" + metric
	}
	return root + cgroupPath + "/" + metric
}

func validCgroupPath(path string) bool {
	if path == "/" {
		return true
	}
	if !strings.HasPrefix(path, "/") || strings.ContainsRune(path, 0) {
		return false
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func splitLines(raw string) []string {
	return strings.FieldsFunc(raw, func(r rune) bool { return r == '\n' || r == '\r' || r == '\v' || r == '\f' })
}
