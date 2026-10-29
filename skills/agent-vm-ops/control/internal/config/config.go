package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const defaultManagementSessionTTLSeconds int64 = 900

var (
	guestToolPattern          = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	trustedImageSourcePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
)

type ForwardAuthConfig struct {
	SocketPath     string `json:"socket_path"`
	PeerUID        int    `json:"peer_uid"`
	IdentityHeader string `json:"identity_header"`
}

type ManagementAuthorityConfig struct {
	SocketPath     string `json:"socket_path"`
	ExpectedWebUID int    `json:"expected_web_uid"`
}

type ManagementUploadConfig struct {
	Enabled          bool   `json:"enabled"`
	MaxBytes         int64  `json:"max_bytes"`
	StagingPath      string `json:"staging_path"`
	MinimumFreeBytes int64  `json:"minimum_free_bytes"`
}

// ManagementConfig is deliberately shared by the authority and Web loaders.
// The Web loader reads only this subtree, while the resident control process
// reads it alongside the legacy control configuration.
type ManagementConfig struct {
	Enabled               bool                      `json:"enabled"`
	ExternalOrigin        string                    `json:"external_origin"`
	ForwardAuth           ForwardAuthConfig         `json:"forward_auth"`
	AllowedAdminSubjects  []string                  `json:"allowed_admin_subjects"`
	WebUID                int                       `json:"web_uid"`
	SessionTTLSeconds     int64                     `json:"session_ttl_seconds"`
	TrustedImageSourceIDs []string                  `json:"trusted_image_source_ids"`
	Authority             ManagementAuthorityConfig `json:"authority"`
	Upload                ManagementUploadConfig    `json:"upload"`
}

type Config struct {
	StateDB                            string           `json:"state_db"`
	Socket                             string           `json:"socket"`
	ImageFingerprint                   string           `json:"image_fingerprint"`
	ImageAlias                         string           `json:"image_alias"`
	Project                            string           `json:"project"`
	Profile                            string           `json:"profile"`
	Network                            string           `json:"network"`
	NetworkACL                         string           `json:"network_acl"`
	Pool                               string           `json:"pool"`
	HostAddress                        string           `json:"host_address"`
	StoragePath                        string           `json:"storage_path"`
	RequiredGuestTools                 []string         `json:"required_guest_tools"`
	VMIPStart                          string           `json:"vm_ip_start"`
	VMIPEnd                            string           `json:"vm_ip_end"`
	GatewayIP                          string           `json:"gateway_ip"`
	TimerDir                           string           `json:"timer_dir"`
	CPUBudget                          int64            `json:"cpu_budget"`
	MemoryBudgetMiB                    int64            `json:"memory_budget_mib"`
	DiskBudgetGiB                      int64            `json:"disk_budget_gib"`
	InstanceLimit                      int64            `json:"instance_limit"`
	CPUOvercommitRatio                 json.Number      `json:"cpu_overcommit_ratio"`
	MemoryOvercommitRatio              json.Number      `json:"memory_overcommit_ratio"`
	CPUStartupPressureFullAvg10Percent float64          `json:"cpu_startup_pressure_full_avg10_percent"`
	PortStart                          int64            `json:"port_start"`
	PortEnd                            int64            `json:"port_end"`
	InitialLeaseSeconds                int64            `json:"initial_lease_seconds"`
	RenewSeconds                       int64            `json:"renew_seconds"`
	MaxLeaseSeconds                    int64            `json:"max_lease_seconds"`
	CacheSeconds                       int64            `json:"cache_seconds"`
	SweepSeconds                       int64            `json:"sweep_seconds"`
	WorkerTimeoutSeconds               int64            `json:"worker_timeout_seconds"`
	DrainTimeoutSeconds                int64            `json:"drain_timeout_seconds"`
	ActivationTimeoutSeconds           int64            `json:"activation_timeout_seconds"`
	ReleaseFencePath                   string           `json:"release_fence_path"`
	MetricsDir                         string           `json:"metrics_dir"`
	AllowCreateDB                      bool             `json:"allow_create_db"`
	Management                         ManagementConfig `json:"management"`
}

type WebConfig struct {
	Management ManagementConfig `json:"management"`
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
		RequiredGuestTools:                 []string{"bun", "python3", "sshd", "rsync", "docker", "growpart"},
		Management: ManagementConfig{
			SessionTTLSeconds: defaultManagementSessionTTLSeconds,
			ForwardAuth:       ForwardAuthConfig{IdentityHeader: "X-Forwarded-User"},
		},
	}
}

func LoadWeb(path string) (WebConfig, error) {
	value := WebConfig{Management: Defaults().Management}
	file, err := os.Open(path)
	if err != nil {
		return value, fmt.Errorf("config_missing: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, fmt.Errorf("config_invalid: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return value, errors.New("config_invalid: trailing JSON")
	}
	if err := value.Management.Validate(true); err != nil {
		return value, err
	}
	return value, nil
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
	if err := c.Management.Validate(false); err != nil {
		return err
	}
	if c.Management.Enabled {
		if c.Management.Authority.SocketPath == c.Socket {
			return errors.New("config_invalid: management.authority.socket_path must be distinct from the control socket")
		}
		if c.Management.ForwardAuth.SocketPath == c.Socket {
			return errors.New("config_invalid: management.forward_auth.socket_path must be distinct from the control socket")
		}
		if c.Management.ForwardAuth.SocketPath == c.Management.Authority.SocketPath {
			return errors.New("config_invalid: management.forward_auth.socket_path must be distinct from the authority socket")
		}
	}
	if err := validateGuestTools(c.RequiredGuestTools); err != nil {
		return err
	}
	return nil
}

func (c ManagementConfig) Validate(webOnly bool) error {
	if !c.Enabled {
		return nil
	}
	if c.ExternalOrigin == "" {
		return errors.New("config_missing: management.external_origin")
	}
	origin, err := url.Parse(c.ExternalOrigin)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || strings.Contains(origin.Host, "*") || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
		return errors.New("config_invalid: management.external_origin must be one HTTPS origin without a path")
	}
	if c.ForwardAuth.IdentityHeader != "X-Forwarded-User" {
		return errors.New("config_invalid: management.forward_auth.identity_header is fixed to X-Forwarded-User")
	}
	if c.ForwardAuth.SocketPath == "" {
		return errors.New("config_missing: management.forward_auth.socket_path")
	}
	if c.ForwardAuth.PeerUID <= 0 {
		return errors.New("config_invalid: management.forward_auth.peer_uid must be a non-root UID")
	}
	if c.WebUID <= 0 {
		return errors.New("config_invalid: management.web_uid must be a non-root UID")
	}
	if c.Authority.SocketPath == "" {
		return errors.New("config_missing: management.authority.socket_path")
	}
	if c.Authority.ExpectedWebUID != c.WebUID {
		return errors.New("config_invalid: management.authority.expected_web_uid must equal management.web_uid")
	}
	if c.ForwardAuth.SocketPath == c.Authority.SocketPath {
		return errors.New("config_invalid: management.forward_auth.socket_path must be distinct from the authority socket")
	}
	if c.SessionTTLSeconds == 0 {
		c.SessionTTLSeconds = defaultManagementSessionTTLSeconds
	}
	if c.SessionTTLSeconds < 60 || c.SessionTTLSeconds > 3600 {
		return errors.New("config_invalid: management.session_ttl_seconds must be between 60 and 3600")
	}
	if err := validateUniqueIDs("management.allowed_admin_subjects", c.AllowedAdminSubjects, true); err != nil {
		return err
	}
	if err := validateUniqueIDs("management.trusted_image_source_ids", c.TrustedImageSourceIDs, false); err != nil {
		return err
	}
	for _, sourceID := range c.TrustedImageSourceIDs {
		if !trustedImageSourcePattern.MatchString(sourceID) {
			return errors.New("config_invalid: management.trusted_image_source_ids contains an invalid Incus remote name")
		}
	}
	if c.Upload.Enabled {
		if c.Upload.MaxBytes <= 0 {
			return errors.New("config_invalid: management.upload.max_bytes must be positive when uploads are enabled")
		}
		if c.Upload.StagingPath == "" || !strings.HasPrefix(c.Upload.StagingPath, "/") {
			return errors.New("config_invalid: management.upload.staging_path must be an absolute path")
		}
		if strings.Trim(c.Upload.StagingPath, "/") == "" {
			return errors.New("config_invalid: management.upload.staging_path must be a private directory")
		}
		if c.Upload.MinimumFreeBytes <= 0 {
			return errors.New("config_invalid: management.upload.minimum_free_bytes must be positive when uploads are enabled")
		}
	} else if c.Upload.MaxBytes < 0 || c.Upload.MinimumFreeBytes < 0 {
		return errors.New("config_invalid: management.upload limits must not be negative")
	}
	if len(c.TrustedImageSourceIDs) > 0 && c.Upload.MinimumFreeBytes <= 0 {
		return errors.New("config_invalid: management.upload.minimum_free_bytes must be positive when trusted image sources are configured")
	}
	if webOnly && c.Authority.SocketPath == "" {
		return errors.New("config_missing: management.authority.socket_path")
	}
	return nil
}

func validateUniqueIDs(name string, values []string, requireNonEmpty bool) error {
	if requireNonEmpty && len(values) == 0 {
		return fmt.Errorf("config_missing: %s", name)
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) || len(value) > 256 {
			return fmt.Errorf("config_invalid: %s contains an empty or oversized value", name)
		}
		if _, exists := seen[value]; exists {
			return fmt.Errorf("config_invalid: %s contains duplicate values", name)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func validateGuestTools(values []string) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !guestToolPattern.MatchString(value) {
			return errors.New("config_invalid: required_guest_tools contains an invalid command name")
		}
		if _, exists := seen[value]; exists {
			return errors.New("config_invalid: required_guest_tools contains duplicate values")
		}
		seen[value] = struct{}{}
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
