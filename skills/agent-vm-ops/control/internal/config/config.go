package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

type Config struct {
	StateDB                   string `json:"state_db"`
	Socket                    string `json:"socket"`
	ImageFingerprint          string `json:"image_fingerprint"`
	ImageAlias                string `json:"image_alias"`
	Project                   string `json:"project"`
	Profile                   string `json:"profile"`
	Network                   string `json:"network"`
	NetworkACL                string `json:"network_acl"`
	Pool                      string `json:"pool"`
	HostAddress               string `json:"host_address"`
	StoragePath               string `json:"storage_path"`
	CPUBudget                 int64  `json:"cpu_budget"`
	MemoryBudgetMiB           int64  `json:"memory_budget_mib"`
	DiskBudgetGiB             int64  `json:"disk_budget_gib"`
	InstanceLimit             int64  `json:"instance_limit"`
	CPUStartupPressurePercent int64  `json:"cpu_startup_pressure_threshold_percent"`
	PortStart                 int64  `json:"port_start"`
	PortEnd                   int64  `json:"port_end"`
	InitialLeaseSeconds       int64  `json:"initial_lease_seconds"`
	RenewSeconds              int64  `json:"renew_seconds"`
	MaxLeaseSeconds           int64  `json:"max_lease_seconds"`
	CacheSeconds              int64  `json:"cache_seconds"`
	SweepSeconds              int64  `json:"sweep_seconds"`
	WorkerTimeoutSeconds      int64  `json:"worker_timeout_seconds"`
	DrainTimeoutSeconds       int64  `json:"drain_timeout_seconds"`
	ActivationTimeoutSeconds  int64  `json:"activation_timeout_seconds"`
	ReleaseFencePath          string `json:"release_fence_path"`
	MetricsDir                string `json:"metrics_dir"`
	AllowCreateDB             bool   `json:"allow_create_db"`
}

func Defaults() Config {
	return Config{
		Socket:                   "/run/agent-vm-native/control.sock",
		InitialLeaseSeconds:      1800,
		RenewSeconds:             900,
		MaxLeaseSeconds:          7200,
		CacheSeconds:             86400,
		SweepSeconds:             1,
		WorkerTimeoutSeconds:     300,
		DrainTimeoutSeconds:      8100,
		ActivationTimeoutSeconds: 300,
		ReleaseFencePath:         "/run/agent-vm-native/release-activation.fence",
		MetricsDir:               "/run/agent-vm-native/metrics",
	}
}

func Load(path string) (Config, error) {
	value := Defaults()
	file, err := os.Open(path)
	if err != nil {
		return value, fmt.Errorf("config_missing: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return value, fmt.Errorf("config_invalid: %w", err)
	}
	if err := value.Validate(); err != nil {
		return value, err
	}
	return value, nil
}

func (c Config) Validate() error {
	if c.StateDB == "" {
		return errors.New("config_missing: state_db")
	}
	if c.Socket == "" {
		return errors.New("config_invalid: socket")
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
	if c.PortStart != 0 && (c.PortStart < 1024 || c.PortStart > 65535) {
		return errors.New("config_invalid: port_start")
	}
	if c.PortEnd != 0 && (c.PortEnd < c.PortStart || c.PortEnd > 65535) {
		return errors.New("config_invalid: port_end")
	}
	if c.CPUBudget < 0 || c.MemoryBudgetMiB < 0 || c.DiskBudgetGiB < 0 || c.InstanceLimit < 0 || c.CPUStartupPressurePercent < 0 || c.CPUStartupPressurePercent > 100 {
		return errors.New("config_invalid: admission limits")
	}
	return nil
}
