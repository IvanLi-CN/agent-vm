package install

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/config"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/manifest"
)

type Inputs struct{ Config, Bundle, Materials string }
type Report struct {
	Mode           string   `json:"mode"`
	SourceRevision string   `json:"source_revision,omitempty"`
	BundleSHA256   string   `json:"bundle_sha256,omitempty"`
	Blockers       []string `json:"blockers"`
	Next           string   `json:"next"`
}

// Preflight never creates directories, records, locks or extracted artifacts.
// Commands are read-only Incus and systemd queries, with the caller's deadline.
func Preflight(ctx context.Context, input Inputs) (Report, error) {
	report := Report{Mode: "fresh", Blockers: []string{}, Next: "rerun install with --apply after reviewing blockers"}
	add := func(err error) {
		if err != nil {
			report.Blockers = append(report.Blockers, err.Error())
		}
	}
	if runtime.GOOS != "linux" || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		add(errors.New("installation requires Linux amd64 or arm64"))
	}
	cfg, configErr := LoadConfig(input.Config)
	add(configErr)
	f, err := os.Open(input.Bundle)
	if err != nil {
		add(errors.New("product bundle unavailable"))
	} else {
		metadata, e := manifest.ReadProductArchive(f)
		f.Close()
		add(e)
		if e == nil {
			report.SourceRevision = metadata.SourceRevision
			_, e = ValidateMaterials(input.Materials, metadata.SourceRevision)
			add(e)
		}
		report.BundleSHA256, e = digestFile(input.Bundle)
		add(e)
	}
	if _, err := os.Lstat("/var/lib/agent-vm-native/active"); err == nil {
		report.Mode = "installed"
		report.Next = "use agent-vm-deploy stage, verify and activate for upgrades"
	} else if !errors.Is(err, os.ErrNotExist) {
		add(errors.New("active installation path cannot be inspected"))
	}
	if report.Mode == "fresh" {
		if _, err := os.Lstat("/var/lib/agent-vm-native/state.sqlite3"); err == nil {
			report.Mode = "legacy"
			report.Next = "delegate the existing migration transaction through the release wrapper"
		} else if !errors.Is(err, os.ErrNotExist) {
			add(errors.New("legacy state cannot be inspected"))
		}
	}
	for _, name := range []string{"systemctl", "systemd-sysusers", "systemd-tmpfiles", "incus"} {
		_, err := exec.LookPath(name)
		if err != nil {
			add(fmt.Errorf("missing prerequisite: %s", name))
		}
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		add(errors.New("running systemd is required"))
	}
	if configErr == nil {
		var blue config.Config
		for _, slot := range []string{"blue", "green"} {
			c, e := config.Load(cfg.Files[slot+"_config"])
			if e != nil {
				add(fmt.Errorf("invalid %s runtime configuration", slot))
				continue
			}
			if c.Slot != slot || !strings.HasPrefix(c.StateDB, "https://") || c.Socket != "/run/agent-vm-native/"+slot+"/app.sock" || !c.Management.Enabled {
				add(fmt.Errorf("%s requires its own slot identity and HTTPS rqlite authority", slot))
			}
			if slot == "blue" {
				blue = c
			} else if c.StateDB != blue.StateDB || c.Project != blue.Project || c.Network != blue.Network || c.Pool != blue.Pool || c.ImageFingerprint != blue.ImageFingerprint {
				add(errors.New("blue and green must share the same business authority and Incus environment"))
			}
			parsed, e := url.Parse(c.StateDB)
			if e != nil || parsed.Hostname() != "127.0.0.1" || parsed.Port() != "4001" || parsed.User == nil || parsed.RawQuery != "" {
				add(errors.New("initial installation requires the authenticated loopback rqlite endpoint"))
			}
		}
		d, e := deployd.Load(cfg.Files["deployd_config"])
		if e != nil {
			add(errors.New("invalid Deployd configuration"))
		}
		if e == nil {
			defaults := deployd.Defaults()
			if d.Socket != defaults.Socket || d.Database != defaults.Database || d.ActivePath != defaults.ActivePath || d.ReleaseRoot != defaults.ReleaseRoot || d.BlueSocket != defaults.BlueSocket || d.GreenSocket != defaults.GreenSocket {
				add(errors.New("installer requires fixed Deployd paths"))
			}
			if deployd.MetricsAddressRequiresTLS(d.MetricsListen) {
				if cfg.Files["metrics_certificate"] == "" || d.MetricsTLS.Certificate != targets["metrics_certificate"].path || d.MetricsTLS.PrivateKey != targets["metrics_private_key"].path || d.MetricsTLS.ClientCA != targets["metrics_client_ca"].path {
					add(errors.New("non-loopback metrics requires the complete fixed metrics TLS input set"))
				}
			}
		}
		for _, name := range []string{"caddy", "rqlite"} {
			cert, ce := protectedRead(cfg.Files[name+"_certificate"])
			key, ke := protectedRead(cfg.Files[name+"_private_key"])
			if ce == nil && ke == nil {
				if _, e := tls.X509KeyPair(cert, key); e != nil {
					add(fmt.Errorf("invalid %s TLS key pair", name))
				}
			}
		}
		if blue.Project != "" {
			for _, args := range [][]string{{"project", "show", blue.Project}, {"profile", "show", blue.Profile, "--project", blue.Project}, {"network", "show", blue.Network, "--project", blue.Project}, {"storage", "show", blue.Pool}, {"image", "info", blue.ImageFingerprint, "--project", blue.Project}} {
				if e := exec.CommandContext(ctx, "incus", args...).Run(); e != nil {
					add(fmt.Errorf("Incus prerequisite failed: %s %s", args[0], args[1]))
				}
			}
		}
	}
	if len(report.Blockers) > 0 {
		return report, errors.New("installation preflight blocked")
	}
	return report, nil
}
