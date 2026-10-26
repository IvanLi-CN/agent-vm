package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type Config struct {
	StateDB                            string      `json:"state_db"`
	Socket                             string      `json:"socket"`
	ImageFingerprint                   string      `json:"image_fingerprint"`
	ImageAlias                         string      `json:"image_alias"`
	Project                            string      `json:"project"`
	Profile                            string      `json:"profile"`
	Network                            string      `json:"network"`
	NetworkACL                         string      `json:"network_acl"`
	Pool                               string      `json:"pool"`
	HostAddress                        string      `json:"host_address"`
	StoragePath                        string      `json:"storage_path"`
	VMIPStart                          string      `json:"vm_ip_start"`
	VMIPEnd                            string      `json:"vm_ip_end"`
	GatewayIP                          string      `json:"gateway_ip"`
	TimerDir                           string      `json:"timer_dir"`
	CPUBudget                          int64       `json:"cpu_budget"`
	MemoryBudgetMiB                    int64       `json:"memory_budget_mib"`
	DiskBudgetGiB                      int64       `json:"disk_budget_gib"`
	InstanceLimit                      int64       `json:"instance_limit"`
	CPUOvercommitRatio                 json.Number `json:"cpu_overcommit_ratio"`
	MemoryOvercommitRatio              json.Number `json:"memory_overcommit_ratio"`
	CPUStartupPressureFullAvg10Percent float64     `json:"cpu_startup_pressure_full_avg10_percent"`
	PortStart                          int64       `json:"port_start"`
	PortEnd                            int64       `json:"port_end"`
	InitialLeaseSeconds                int64       `json:"initial_lease_seconds"`
	RenewSeconds                       int64       `json:"renew_seconds"`
	MaxLeaseSeconds                    int64       `json:"max_lease_seconds"`
	CacheSeconds                       int64       `json:"cache_seconds"`
	SweepSeconds                       int64       `json:"sweep_seconds"`
	WorkerTimeoutSeconds               int64       `json:"worker_timeout_seconds"`
	DrainTimeoutSeconds                int64       `json:"drain_timeout_seconds"`
	ActivationTimeoutSeconds           int64       `json:"activation_timeout_seconds"`
	ReleaseFencePath                   string      `json:"release_fence_path"`
	MetricsDir                         string      `json:"metrics_dir"`
	AllowCreateDB                      bool        `json:"allow_create_db"`
}

func Defaults() Config {
	return Config{
		Socket:                             "/run/agent-vm-native/control.sock",
		InitialLeaseSeconds:                1800,
		RenewSeconds:                       900,
		MaxLeaseSeconds:                    7200,
		CacheSeconds:                       86400,
		CPUOvercommitRatio:                 json.Number("4"),
		MemoryOvercommitRatio:              json.Number("4"),
		CPUStartupPressureFullAvg10Percent: 20,
		SweepSeconds:                       1,
		WorkerTimeoutSeconds:               300,
		DrainTimeoutSeconds:                8100,
		ActivationTimeoutSeconds:           300,
		ReleaseFencePath:                   "/run/agent-vm-native/release-activation.fence",
		MetricsDir:                         "/run/agent-vm-native/metrics",
		TimerDir:                           "/run/agent-vm-native/timers",
	}
}

func Load(path string) (Config, error) {
	value := Defaults()
	file, err := os.Open(path)
	if err != nil {
		return value, fmt.Errorf("config_missing: %w", err)
	}
	defer file.Close()
	var raw map[string]json.RawMessage
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&raw); err != nil {
		return value, fmt.Errorf("config_invalid: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return value, errors.New("config_invalid: trailing JSON")
	}
	if _, exists := raw["cpu_startup_pressure_threshold_percent"]; exists {
		return value, errors.New("config_invalid: use cpu_startup_pressure_full_avg10_percent")
	}
	missing := []string{}
	for _, key := range requiredFields {
		entry, exists := raw[key]
		if !exists || len(entry) == 0 || string(entry) == "null" || string(entry) == `""` {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return value, fmt.Errorf("config_missing: required environment configuration is missing: %s", strings.Join(missing, ", "))
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return value, fmt.Errorf("config_invalid: %w", err)
	}
	if err := json.Unmarshal(encoded, &value); err != nil {
		return value, fmt.Errorf("config_invalid: %w", err)
	}
	if err := value.Validate(); err != nil {
		return value, err
	}
	return value, nil
}

func (c Config) Validate() error {
	missing := []string{}
	for key, value := range map[string]string{
		"state_db": c.StateDB, "image_alias": c.ImageAlias, "image_fingerprint": c.ImageFingerprint,
		"project": c.Project, "profile": c.Profile, "network": c.Network, "network_acl": c.NetworkACL,
		"pool": c.Pool, "host_address": c.HostAddress, "storage_path": c.StoragePath,
		"vm_ip_start": c.VMIPStart, "vm_ip_end": c.VMIPEnd, "gateway_ip": c.GatewayIP, "timer_dir": c.TimerDir,
	} {
		if value == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("config_missing: required environment configuration is missing: %s", strings.Join(missing, ", "))
	}
	if c.Socket == "" {
		return errors.New("config_invalid: socket")
	}
	if c.ReleaseFencePath == "" {
		return errors.New("config_invalid: release_fence_path")
	}
	if c.ReleaseFencePath != "/run/agent-vm-native/release-activation.fence" {
		return errors.New("config_invalid: release_fence_path must use the fixed activation fence")
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(c.ImageFingerprint) {
		return errors.New("config_invalid: image_fingerprint must be a 64-character lowercase hex fingerprint")
	}
	if !regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`).MatchString(c.NetworkACL) {
		return errors.New("config_invalid: network_acl")
	}
	for key, value := range map[string]string{"host_address": c.HostAddress, "vm_ip_start": c.VMIPStart, "vm_ip_end": c.VMIPEnd, "gateway_ip": c.GatewayIP} {
		if parsed := net.ParseIP(value); parsed == nil || parsed.To4() == nil {
			return fmt.Errorf("config_invalid: %s must be an IPv4 address", key)
		}
	}
	start := ipNumber(c.VMIPStart)
	end := ipNumber(c.VMIPEnd)
	if start > end {
		return errors.New("config_invalid: vm_ip_start must not exceed vm_ip_end")
	}
	if c.InitialLeaseSeconds <= 0 || c.RenewSeconds <= 0 || c.MaxLeaseSeconds <= 0 || c.CacheSeconds <= 0 {
		return errors.New("config_invalid: lease and cache durations must be positive")
	}
	if c.RenewSeconds > c.MaxLeaseSeconds {
		return errors.New("config_invalid: renew_seconds exceeds max_lease_seconds")
	}
	if c.SweepSeconds <= 0 || c.WorkerTimeoutSeconds <= 0 || c.DrainTimeoutSeconds <= 0 || c.ActivationTimeoutSeconds <= 0 {
		return errors.New("config_invalid: scheduler and timeout values must be positive")
	}
	if c.PortStart < 1024 || c.PortStart > 65535 {
		return errors.New("config_invalid: port_start")
	}
	if c.PortEnd < c.PortStart || c.PortEnd > 65535 {
		return errors.New("config_invalid: port_end")
	}
	if c.CPUBudget < 0 || c.MemoryBudgetMiB < 0 || c.DiskBudgetGiB < 0 || c.InstanceLimit < 0 {
		return errors.New("config_invalid: admission limits")
	}
	if _, _, err := decimalRatio(c.CPUOvercommitRatio, "cpu_overcommit_ratio"); err != nil {
		return err
	}
	if _, _, err := decimalRatio(c.MemoryOvercommitRatio, "memory_overcommit_ratio"); err != nil {
		return err
	}
	if math.IsNaN(c.CPUStartupPressureFullAvg10Percent) || math.IsInf(c.CPUStartupPressureFullAvg10Percent, 0) || c.CPUStartupPressureFullAvg10Percent < 0 || c.CPUStartupPressureFullAvg10Percent > 100 {
		return errors.New("config_invalid: cpu_startup_pressure_full_avg10_percent must be between zero and 100")
	}
	return nil
}

var requiredFields = []string{"project", "profile", "image_alias", "image_fingerprint", "network", "network_acl", "pool", "host_address", "port_start", "port_end", "vm_ip_start", "vm_ip_end", "gateway_ip", "storage_path", "state_db", "timer_dir"}

func ipNumber(value string) uint32 {
	parsed := net.ParseIP(value).To4()
	if parsed == nil {
		return 0
	}
	return uint32(parsed[0])<<24 | uint32(parsed[1])<<16 | uint32(parsed[2])<<8 | uint32(parsed[3])
}

// LogicalLimit returns floor(base * ratio), matching the Bun controller's
// decimal admission policy without converting a configuration number to a
// binary floating-point value.
func LogicalLimit(base int64, ratio json.Number) (int64, error) {
	if base < 0 {
		return 0, errors.New("config_invalid: admission base must not be negative")
	}
	if base == 0 {
		return 0, nil
	}
	numerator, denominator, err := decimalRatio(ratio, "ratio")
	if err != nil {
		return 0, err
	}
	product := new(big.Int).Mul(big.NewInt(base), numerator)
	product.Quo(product, denominator)
	if !product.IsInt64() {
		return 0, errors.New("config_invalid: logical admission limit exceeds int64")
	}
	return product.Int64(), nil
}

func decimalRatio(value json.Number, name string) (*big.Int, *big.Int, error) {
	raw := strings.TrimSpace(value.String())
	if raw == "" {
		return nil, nil, fmt.Errorf("config_invalid: %s must be a finite number at least one", name)
	}
	parts := strings.Split(strings.ToLower(raw), "e")
	if len(parts) > 2 {
		return nil, nil, fmt.Errorf("config_invalid: %s must be a finite number at least one", name)
	}
	mantissa := parts[0]
	exponent := 0
	if len(parts) == 2 {
		parsed, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, nil, fmt.Errorf("config_invalid: %s must be a finite number at least one", name)
		}
		exponent = parsed
	}
	if strings.HasPrefix(mantissa, "+") || strings.HasPrefix(mantissa, "-") {
		return nil, nil, fmt.Errorf("config_invalid: %s must be a finite number at least one", name)
	}
	decimal := strings.Split(mantissa, ".")
	if len(decimal) > 2 || decimal[0] == "" || (len(decimal) == 2 && decimal[1] == "") {
		return nil, nil, fmt.Errorf("config_invalid: %s must be a finite number at least one", name)
	}
	if !regexp.MustCompile(`^[0-9]+$`).MatchString(decimal[0]) || (len(decimal) == 2 && !regexp.MustCompile(`^[0-9]+$`).MatchString(decimal[1])) {
		return nil, nil, fmt.Errorf("config_invalid: %s must be a finite number at least one", name)
	}
	digits := decimal[0]
	if len(decimal) == 2 {
		digits += decimal[1]
	}
	numerator := new(big.Int)
	if _, ok := numerator.SetString(digits, 10); !ok {
		return nil, nil, fmt.Errorf("config_invalid: %s must be a finite number at least one", name)
	}
	scale := 0
	if len(decimal) == 2 {
		scale = len(decimal[1])
	}
	scale -= exponent
	denominator := big.NewInt(1)
	if scale >= 0 {
		denominator.Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)
	} else {
		numerator.Mul(numerator, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(-scale)), nil))
	}
	if numerator.Cmp(denominator) < 0 {
		return nil, nil, fmt.Errorf("config_invalid: %s must be a finite number at least one", name)
	}
	return numerator, denominator, nil
}
