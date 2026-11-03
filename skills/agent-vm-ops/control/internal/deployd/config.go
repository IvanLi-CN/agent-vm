package deployd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type MetricsTLSConfig struct {
	Certificate string `json:"certificate"`
	PrivateKey  string `json:"private_key"`
	ClientCA    string `json:"client_ca"`
}

type MigrationConfig struct {
	LegacySocket        string `json:"legacy_socket"`
	LegacyUnit          string `json:"legacy_unit"`
	LegacyStateDB       string `json:"legacy_state_db"`
	RQLiteURL           string `json:"rqlite_url"`
	RQLiteUnit          string `json:"rqlite_unit"`
	RQLiteCAFile        string `json:"rqlite_ca_file"`
	RQLiteAuthFile      string `json:"rqlite_auth_file"`
	RecoveryDirectory   string `json:"recovery_directory"`
	DrainTimeoutSeconds int64  `json:"drain_timeout_seconds"`
	WriterCheckSeconds  int64  `json:"writer_check_seconds"`
}

type Config struct {
	Socket         string           `json:"socket"`
	LockPath       string           `json:"lock_path"`
	MetricsListen  string           `json:"metrics_listen"`
	MetricsTLS     MetricsTLSConfig `json:"metrics_tls"`
	NetworkACL     string           `json:"network_acl"`
	Database       string           `json:"database"`
	ActivePath     string           `json:"active_path"`
	ReleaseRoot    string           `json:"release_root"`
	BlueSocket     string           `json:"blue_socket"`
	GreenSocket    string           `json:"green_socket"`
	LeaseSeconds   int64            `json:"lease_seconds"`
	RetentionDays  int64            `json:"retention_days"`
	ObserveSeconds int64            `json:"observe_seconds"`
	Migration      MigrationConfig  `json:"migration"`
}

func Defaults() Config {
	return Config{
		Socket:         "/run/agent-vm-native/deployd.sock",
		LockPath:       "/run/agent-vm-native/deployd.lock",
		MetricsListen:  "127.0.0.1:9911",
		Database:       "/var/lib/agent-vm-native/deployment/deployment.sqlite3",
		ActivePath:     "/var/lib/agent-vm-native/active",
		ReleaseRoot:    "/var/lib/agent-vm-native",
		BlueSocket:     "/run/agent-vm-native/blue/app.sock",
		GreenSocket:    "/run/agent-vm-native/green/app.sock",
		LeaseSeconds:   15,
		RetentionDays:  30,
		ObserveSeconds: 5,
		Migration: MigrationConfig{
			LegacySocket:        "/run/agent-vm-native/app.sock",
			LegacyUnit:          "agent-vm-native.service",
			LegacyStateDB:       "/var/lib/agent-vm-native/state.sqlite3",
			RQLiteUnit:          "agent-vm-rqlite.service",
			RQLiteCAFile:        "/etc/agent-vm-native/tls/rqlite/server.crt",
			RQLiteAuthFile:      "/etc/agent-vm-native/rqlite/auth.json",
			RecoveryDirectory:   "/var/lib/agent-vm-native/migration",
			DrainTimeoutSeconds: 8100,
			WriterCheckSeconds:  1,
		},
	}
}

func Load(path string) (Config, error) {
	value := Defaults()
	file, err := os.Open(path)
	if err != nil {
		return value, fmt.Errorf("deployd config missing: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, fmt.Errorf("deployd config invalid: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return value, errors.New("deployd config invalid: trailing JSON")
	}
	if err := value.Validate(); err != nil {
		return value, err
	}
	return value, nil
}

func (config Config) Validate() error {
	if config.LockPath != Defaults().LockPath {
		return fmt.Errorf("deployd config invalid: lock_path must be the host singleton path %s", Defaults().LockPath)
	}
	for name, value := range map[string]string{
		"socket": config.Socket, "lock_path": config.LockPath, "database": config.Database, "active_path": config.ActivePath,
		"release_root": config.ReleaseRoot, "blue_socket": config.BlueSocket, "green_socket": config.GreenSocket,
	} {
		if value == "" || !filepath.IsAbs(value) {
			return fmt.Errorf("deployd config invalid: %s must be an absolute path", name)
		}
	}
	if config.BlueSocket == config.GreenSocket {
		return errors.New("deployd config invalid: slot sockets must be distinct")
	}
	if config.LeaseSeconds < 3 || config.RetentionDays <= 0 || config.ObserveSeconds <= 0 {
		return errors.New("deployd config invalid: lease, retention and observe intervals are invalid")
	}
	if err := config.Migration.Validate(); err != nil {
		return err
	}
	if config.Socket == config.LockPath || config.Socket == config.Database || config.Socket == config.ActivePath || config.LockPath == config.Database || config.LockPath == config.ActivePath {
		return errors.New("deployd config invalid: socket or lock path collides with a state path")
	}
	host, _, err := net.SplitHostPort(config.MetricsListen)
	if err != nil || host == "" {
		return errors.New("deployd config invalid: metrics_listen must be host:port")
	}
	if !isLoopbackHost(host) {
		if config.NetworkACL == "" || config.MetricsTLS.Certificate == "" || config.MetricsTLS.PrivateKey == "" || config.MetricsTLS.ClientCA == "" {
			return errors.New("deployd config invalid: non-loopback metrics requires mTLS and network_acl")
		}
		if _, err := parseNetworkACL(config.NetworkACL); err != nil {
			return fmt.Errorf("deployd config invalid: network_acl: %w", err)
		}
		for name, value := range map[string]string{"certificate": config.MetricsTLS.Certificate, "private_key": config.MetricsTLS.PrivateKey, "client_ca": config.MetricsTLS.ClientCA} {
			if !filepath.IsAbs(value) {
				return fmt.Errorf("deployd config invalid: metrics_tls.%s must be absolute", name)
			}
		}
	} else if config.NetworkACL != "" {
		if _, err := parseNetworkACL(config.NetworkACL); err != nil {
			return fmt.Errorf("deployd config invalid: network_acl: %w", err)
		}
	}
	tlsConfigured := config.MetricsTLS.Certificate != "" || config.MetricsTLS.PrivateKey != "" || config.MetricsTLS.ClientCA != ""
	if tlsConfigured && (config.MetricsTLS.Certificate == "" || config.MetricsTLS.PrivateKey == "" || config.MetricsTLS.ClientCA == "") {
		return errors.New("deployd config invalid: metrics_tls requires certificate, private_key and client_ca together")
	}
	return nil
}

func (config MigrationConfig) Validate() error {
	for name, value := range map[string]string{
		"migration.legacy_socket":      config.LegacySocket,
		"migration.legacy_state_db":    config.LegacyStateDB,
		"migration.rqlite_ca_file":     config.RQLiteCAFile,
		"migration.rqlite_auth_file":   config.RQLiteAuthFile,
		"migration.recovery_directory": config.RecoveryDirectory,
	} {
		if value == "" || !filepath.IsAbs(value) {
			return fmt.Errorf("deployd config invalid: %s must be an absolute path", name)
		}
	}
	if config.LegacyUnit != "agent-vm-native.service" {
		return errors.New("deployd config invalid: migration.legacy_unit must be agent-vm-native.service")
	}
	if config.RQLiteUnit != "agent-vm-rqlite.service" {
		return errors.New("deployd config invalid: migration.rqlite_unit must be agent-vm-rqlite.service")
	}
	if config.RQLiteURL != "" {
		parsed, err := neturl.Parse(config.RQLiteURL)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return errors.New("deployd config invalid: migration.rqlite_url must be an HTTP(S) URL")
		}
	}
	if config.DrainTimeoutSeconds <= 0 || config.WriterCheckSeconds <= 0 {
		return errors.New("deployd config invalid: migration timeouts are invalid")
	}
	return nil
}

func (config Config) LeaseDuration() time.Duration {
	return time.Duration(config.LeaseSeconds) * time.Second
}
func (config Config) Retention() time.Duration {
	return time.Duration(config.RetentionDays) * 24 * time.Hour
}
func (config Config) ObserveInterval() time.Duration {
	return time.Duration(config.ObserveSeconds) * time.Second
}

func isLoopbackHost(host string) bool {
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	parsed := net.ParseIP(host)
	return parsed != nil && parsed.IsLoopback()
}

func parseNetworkACL(value string) ([]*net.IPNet, error) {
	parts := strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' })
	if len(parts) == 0 {
		return nil, errors.New("must contain at least one IP or CIDR")
	}
	networks := make([]*net.IPNet, 0, len(parts))
	for _, part := range parts {
		if ip := net.ParseIP(part); ip != nil {
			bits := 128
			if ip.To4() != nil {
				bits = 32
				ip = ip.To4()
			}
			networks = append(networks, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		_, network, err := net.ParseCIDR(part)
		if err != nil {
			return nil, fmt.Errorf("invalid entry %q", part)
		}
		networks = append(networks, network)
	}
	return networks, nil
}
