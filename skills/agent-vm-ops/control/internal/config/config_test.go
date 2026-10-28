package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validConfig() Config {
	value := Defaults()
	value.StateDB = "/var/lib/agent-vm-native/state.sqlite3"
	value.ImageFingerprint = strings.Repeat("a", 64)
	value.ImageAlias = "agent-image"
	value.Project = "agents"
	value.Profile = "default"
	value.Network = "agent-net"
	value.NetworkACL = "agent-vm-acl"
	value.Pool = "default"
	value.HostAddress = "192.0.2.1"
	value.StoragePath = "/var/lib/agent-vm-native"
	value.VMIPStart = "192.0.2.10"
	value.VMIPEnd = "192.0.2.200"
	value.GatewayIP = "192.0.2.1"
	value.PortStart = 22000
	value.PortEnd = 22999
	return value
}

func TestValidateRequiresDeploymentIdentity(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatal(err)
	}
	missing := validConfig()
	missing.HostAddress = ""
	if err := missing.Validate(); err == nil || !strings.Contains(err.Error(), "config_missing") {
		t.Fatalf("missing host address error = %v", err)
	}
	invalid := validConfig()
	invalid.ImageFingerprint = strings.Repeat("A", 64)
	if err := invalid.Validate(); err == nil || !strings.Contains(err.Error(), "config_invalid") {
		t.Fatalf("invalid image fingerprint error = %v", err)
	}
}

func TestLoadRejectsMissingRawEnvironmentFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"state_db":"/tmp/state.sqlite3"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "config_missing") {
		t.Fatalf("missing config error = %v", err)
	}
}

func TestLogicalLimitUsesExactDecimalRatio(t *testing.T) {
	limit, err := LogicalLimit(15, json.Number("1.1"))
	if err != nil {
		t.Fatal(err)
	}
	if limit != 16 {
		t.Fatalf("logical limit = %d, want 16", limit)
	}
	if _, err := LogicalLimit(15, json.Number("0.9")); err == nil {
		t.Fatal("ratio below one was accepted")
	}
	if limit, err := LogicalLimit(15, json.Number("1e1")); err != nil || limit != 150 {
		t.Fatalf("exponent ratio = %d, %v", limit, err)
	}
}

func TestCanonicalAdmissionFieldsValidate(t *testing.T) {
	value := validConfig()
	value.CPUOvercommitRatio = json.Number("4")
	value.MemoryOvercommitRatio = json.Number("2")
	value.CPUStartupPressureFullAvg10Percent = 50
	if err := value.Validate(); err != nil {
		t.Fatal(err)
	}
	value.CPUOvercommitRatio = json.Number("0.9")
	if err := value.Validate(); err == nil || !strings.Contains(err.Error(), "cpu_overcommit_ratio") {
		t.Fatalf("invalid CPU ratio error = %v", err)
	}
}

func TestLoadPreservesCanonicalAdmissionFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	value := validConfig()
	value.CPUOvercommitRatio = json.Number("1.1")
	value.MemoryOvercommitRatio = json.Number("2")
	value.CPUStartupPressureFullAvg10Percent = 50
	value.CreateCostBytes = 7 * 1024 * 1024 * 1024
	value.ResumeCostBytes = 2 * 1024 * 1024 * 1024
	value.ResizeCostBytes = 3 * 1024 * 1024 * 1024
	value.DiskLowBytes = 11 * 1024 * 1024 * 1024
	value.DiskTargetBytes = 22 * 1024 * 1024 * 1024
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CPUOvercommitRatio.String() != "1.1" || loaded.MemoryOvercommitRatio.String() != "2" || loaded.CPUStartupPressureFullAvg10Percent != 50 ||
		loaded.CreateCostBytes != value.CreateCostBytes || loaded.ResumeCostBytes != value.ResumeCostBytes || loaded.ResizeCostBytes != value.ResizeCostBytes ||
		loaded.DiskLowBytes != value.DiskLowBytes || loaded.DiskTargetBytes != value.DiskTargetBytes {
		t.Fatalf("loaded admission policy = %#v", loaded)
	}
}

func TestManagementConfigRequiresSeparateAuthenticatedBoundaries(t *testing.T) {
	value := validConfig()
	value.Management = ManagementConfig{
		Enabled: true, ExternalOrigin: "https://console.example",
		ForwardAuth:          ForwardAuthConfig{SocketPath: "/run/agent-vm-native/proxy.sock", PeerUID: 1001, IdentityHeader: "X-Forwarded-User"},
		AllowedAdminSubjects: []string{"admin@example"}, WebUID: 1002,
		SessionTTLSeconds: 900,
		Authority:         ManagementAuthorityConfig{SocketPath: "/run/agent-vm-native/authority.sock", ExpectedWebUID: 1002},
	}
	if err := value.Validate(); err != nil {
		t.Fatal(err)
	}
	value.Management.Authority.SocketPath = value.Socket
	if err := value.Validate(); err == nil || !strings.Contains(err.Error(), "distinct from the control socket") {
		t.Fatalf("shared control socket error = %v", err)
	}
	value = validConfig()
	value.Management = ManagementConfig{
		Enabled: true, ExternalOrigin: "https://console.example", ForwardAuth: ForwardAuthConfig{SocketPath: "/run/proxy.sock", PeerUID: 1001, IdentityHeader: "X-Forwarded-User"},
		AllowedAdminSubjects: []string{"admin@example"}, WebUID: 1002, Authority: ManagementAuthorityConfig{SocketPath: "/run/authority.sock", ExpectedWebUID: 1002},
		Upload: ManagementUploadConfig{Enabled: true, MaxBytes: 1024, StagingPath: "/", MinimumFreeBytes: 512},
	}
	if err := value.Validate(); err == nil || !strings.Contains(err.Error(), "private directory") {
		t.Fatalf("root staging error = %v", err)
	}
}

func TestManagementConfigRejectsUnsafeIncusRemoteNames(t *testing.T) {
	value := validConfig()
	value.Management = ManagementConfig{
		Enabled: true, ExternalOrigin: "https://console.example",
		ForwardAuth:          ForwardAuthConfig{SocketPath: "/run/proxy.sock", PeerUID: 1001, IdentityHeader: "X-Forwarded-User"},
		AllowedAdminSubjects: []string{"admin@example"}, WebUID: 1002,
		TrustedImageSourceIDs: []string{"approved/remote"},
		Authority:             ManagementAuthorityConfig{SocketPath: "/run/authority.sock", ExpectedWebUID: 1002},
	}
	if err := value.Validate(); err == nil || !strings.Contains(err.Error(), "invalid Incus remote name") {
		t.Fatalf("unsafe remote error = %v", err)
	}
}

func TestManagementConfigRequiresPoolReserveForTrustedImages(t *testing.T) {
	value := validConfig()
	value.Management = ManagementConfig{
		Enabled: true, ExternalOrigin: "https://console.example",
		ForwardAuth:          ForwardAuthConfig{SocketPath: "/run/proxy.sock", PeerUID: 1001, IdentityHeader: "X-Forwarded-User"},
		AllowedAdminSubjects: []string{"admin@example"}, WebUID: 1002,
		TrustedImageSourceIDs: []string{"approved"},
		Authority:             ManagementAuthorityConfig{SocketPath: "/run/authority.sock", ExpectedWebUID: 1002},
	}
	if err := value.Validate(); err == nil || !strings.Contains(err.Error(), "minimum_free_bytes") {
		t.Fatalf("missing trusted image reserve error = %v", err)
	}
	value.Management.Upload.MinimumFreeBytes = 512
	if err := value.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestConfigRejectsUnsafeRequiredGuestTools(t *testing.T) {
	value := validConfig()
	value.RequiredGuestTools = []string{"bun;id"}
	if err := value.Validate(); err == nil || !strings.Contains(err.Error(), "required_guest_tools") {
		t.Fatalf("unsafe guest tool error = %v", err)
	}
}
