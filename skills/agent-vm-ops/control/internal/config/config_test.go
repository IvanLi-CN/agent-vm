package config

import (
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
