package deployd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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

type Config struct {
	Socket         string           `json:"socket"`
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
}

func Defaults() Config {
	return Config{
		Socket:         "/run/agent-vm-native/deployd.sock",
		MetricsListen:  "127.0.0.1:9911",
		Database:       "/var/lib/agent-vm-native/deployment/deployment.sqlite3",
		ActivePath:     "/var/lib/agent-vm-native/active",
		ReleaseRoot:    "/var/lib/agent-vm-native",
		BlueSocket:     "/run/agent-vm-native/blue/app.sock",
		GreenSocket:    "/run/agent-vm-native/green/app.sock",
		LeaseSeconds:   15,
		RetentionDays:  30,
		ObserveSeconds: 5,
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
	for name, value := range map[string]string{
		"socket": config.Socket, "database": config.Database, "active_path": config.ActivePath,
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
	if config.Socket == config.Database || config.Socket == config.ActivePath {
		return errors.New("deployd config invalid: socket path collides with a state path")
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
