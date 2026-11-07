package install

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Config references operator-owned files; credential bytes never enter reports
// or the installation journal.
type Config struct {
	SchemaVersion int               `json:"schema_version"`
	Files         map[string]string `json:"files"`
}
type target struct {
	path, group string
	mode        os.FileMode
}

var targets = map[string]target{
	"metrics_certificate": {"/etc/agent-vm-native/tls/metrics/server.crt", "agent-vm-web", 0640},
	"metrics_private_key": {"/etc/agent-vm-native/tls/metrics/server.key", "agent-vm-web", 0640},
	"metrics_client_ca":   {"/etc/agent-vm-native/tls/metrics/client-ca.crt", "agent-vm-web", 0640},
	"blue_config":         {"/etc/agent-vm-native/slots/blue/config.json", "agent-vm-web", 0640},
	"green_config":        {"/etc/agent-vm-native/slots/green/config.json", "agent-vm-web", 0640},
	"deployd_config":      {"/etc/agent-vm-native/deployd.json", "agent-vm-web", 0640},
	"caddy_config":        {"/etc/agent-vm-native/Caddyfile", "agent-vm-web", 0640},
	"caddy_environment":   {"/etc/agent-vm-native/caddy.env", "agent-vm-web", 0640},
	"rqlite_auth":         {"/etc/agent-vm-native/rqlite/auth.json", "agent-vm-rqlite", 0640},
	"rqlite_certificate":  {"/etc/agent-vm-native/tls/rqlite/server.crt", "agent-vm-rqlite", 0640},
	"rqlite_private_key":  {"/etc/agent-vm-native/tls/rqlite/server.key", "agent-vm-rqlite", 0640},
	"caddy_certificate":   {"/etc/agent-vm-native/tls/caddy/server.crt", "agent-vm-web", 0640},
	"caddy_private_key":   {"/etc/agent-vm-native/tls/caddy/server.key", "agent-vm-web", 0640},
}

func LoadConfig(path string) (Config, error) {
	var cfg Config
	data, err := protectedRead(path)
	if err != nil {
		return cfg, err
	}
	if err := decodeStrict(data, &cfg); err != nil {
		return cfg, err
	}
	if cfg.SchemaVersion != 1 || (len(cfg.Files) != len(targets)-3 && len(cfg.Files) != len(targets)) {
		return cfg, errors.New("installation config requires schema 1 and the complete fixed file set")
	}
	for name := range cfg.Files {
		if _, ok := targets[name]; !ok {
			return cfg, fmt.Errorf("unknown installation input: %s", name)
		}
	}
	for name := range targets {
		if strings.HasPrefix(name, "metrics_") && cfg.Files[name] == "" {
			continue
		}
		if _, err := protectedRead(cfg.Files[name]); err != nil {
			return cfg, fmt.Errorf("invalid protected input %s: %w", name, err)
		}
	}
	return cfg, nil
}
func decodeStrict(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}
func protectedRead(path string) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("protected input requires a canonical absolute path")
	}
	f, err := materialFile(filepath.Dir(path), filepath.Base(path))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Geteuid() || info.Mode().Perm()&0027 != 0 || info.Size() > 4<<20 {
		return nil, errors.New("input must be owned by the invoking user, protected from other users and at most 4 MiB")
	}
	return io.ReadAll(io.LimitReader(f, 4<<20+1))
}
func digestFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
