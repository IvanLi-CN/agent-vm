package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/config"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/manifest"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

// initializeState is a local installation operation. It does not start a
// scheduler, grant roles, change the active link, or create a SQLite source.
func initializeState(args []string) error {
	flags := flag.NewFlagSet("initialize-state", flag.ContinueOnError)
	resume := flags.Bool("resume", false, "verify and resume an interrupted empty-authority initialization")
	configPath := flags.String("config", "/etc/agent-vm-native/slots/blue/config.json", "slot configuration")
	manifestPath := flags.String("manifest", "/var/lib/agent-vm-native/slots/blue/release-manifest.json", "verified slot manifest")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("initialize-state accepts flags only")
	}
	if *configPath != "/etc/agent-vm-native/slots/blue/config.json" || *manifestPath != slotManifestPath("blue") {
		return errors.New("initialize-state requires fixed initial blue slot paths")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if cfg.Slot != "blue" {
		return errors.New("initialize-state requires initial blue slot identity")
	}
	if err := config.ValidateInstallationAuthority(cfg.StateDB); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return errors.New("initialize-state requires local root")
	}
	for _, path := range []string{"/var/lib/agent-vm-native/active", "/var/lib/agent-vm-native/state.sqlite3"} {
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("fresh initialization requires absent legacy state and active link: %s", path)
		}
	}
	release, err := manifest.LoadMetadata(*manifestPath)
	if err != nil {
		return err
	}
	if !release.ArtifactsVerified || len(release.StateWrites) == 0 {
		return errors.New("initialize-state requires a verified management release")
	}
	open := state.OpenFreshRQLite
	if *resume {
		open = state.ResumeFreshRQLite
	}
	store, err := open(context.Background(), cfg.StateDB)
	if err != nil {
		return fmt.Errorf("initialize rqlite: %w", err)
	}
	defer store.Close()
	if err := store.EnsureManagementSchema(context.Background()); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"initialized": true, "business_authority": "rqlite", "source_revision": release.SourceRevision})
}
