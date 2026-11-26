package install

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// hostUnitAssets are the unit files every deployment must be able to refresh.
// They ship inside the candidate bundle and carry no operator data, so a
// release may replace them in place. Slot products, slot drop-ins and the
// deployd account drop-in stay untouched here.
var hostUnitAssets = []string{
	"agent-vm-native@.service",
	"agent-vm-deployd.service",
	"agent-vm-deployd.socket",
	"agent-vm-rqlite.service",
	"agent-vm-caddy.service",
	"agent-vm-observe.socket",
	"agent-vm-observe@.service",
	"agent-vm-deploy-host.socket",
	"agent-vm-deploy-host@.service",
	"agent-vm-slot-relay.socket",
	"agent-vm-slot-relay@.service",
	"agent-vm-request.socket",
	"agent-vm-request@.service",
}

// hostSocketAssets are the socket units whose listening descriptor systemd
// discards when their file changes underneath a running unit.
var hostSocketAssets = []string{
	"agent-vm-deploy-host.socket",
	"agent-vm-slot-relay.socket",
}

// PublishHostUnits refreshes the singleton and helper units from a verified
// product tree. Existing installs only ever ran `install`, so an upgrade left
// the singleton template at whatever generation the host originally carried.
// The slot drop-ins do not override ExecStartPre/ExecStartPost, so a stale
// template kept starting the previous maintenance binary, and a stale request
// unit kept NO_NEW_PRIVS on an adapter that must setuid into the slot account.
//
// Idempotent: equal protected files are left alone, so this is safe to run
// before every transaction.
const outputUnitRoot = "/etc/systemd/system"

func PublishHostUnits(product string) ([]string, error) {
	return publishHostUnits(product, outputUnitRoot)
}

func publishHostUnits(product, targetRoot string) ([]string, error) {
	return publishHostUnitsInto(product, targetRoot, true)
}

func publishHostUnitsInto(product, targetRoot string, protectParents bool) ([]string, error) {
	units := filepath.Join(product, "systemd")
	var changed []string
	for _, name := range hostUnitAssets {
		source := filepath.Join(units, name)
		if _, err := os.Lstat(source); errors.Is(err, os.ErrNotExist) {
			// The product bundle carries a subset of the units; the rest live in
			// deployment materials, which `update` does not take. Publish what
			// the bundle has and leave the others to `install`.
			continue
		} else if err != nil {
			return changed, fmt.Errorf("inspect %s: %w", name, err)
		}
		target := filepath.Join(targetRoot, name)
		replaced, err := replaceUnitAsset(source, target, protectParents)
		if err != nil {
			return changed, fmt.Errorf("publish %s: %w", name, err)
		}
		if replaced {
			changed = append(changed, name)
		}
	}
	return changed, nil
}

// replaceUnitAsset publishes source over target unless the bytes already match.
// Unlike installFile it overwrites, because these paths belong to the release.
func replaceUnitAsset(source, target string, protectParents bool) (bool, error) {
	next, err := os.ReadFile(source)
	if err != nil {
		return false, err
	}
	if current, err := os.ReadFile(target); err == nil && string(current) == string(next) {
		return false, nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if protectParents {
		if err := safeParents(filepath.Dir(target)); err != nil {
			return false, err
		}
	} else if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return false, err
	}
	file, err := os.CreateTemp(filepath.Dir(target), ".unit-")
	if err != nil {
		return false, err
	}
	temp := file.Name()
	defer os.Remove(temp)
	if _, err := file.Write(next); err != nil {
		file.Close()
		return false, err
	}
	// Chown needs privilege; a non-root run keeps the invoking owner and the
	// real install always runs as root.
	if err := errors.Join(file.Chmod(0644), file.Sync(), file.Close()); err != nil {
		return false, err
	}
	_ = file.Chown(0, 0)
	if err := os.Rename(temp, target); err != nil {
		return false, err
	}
	return true, syncDir(filepath.Dir(target))
}

// publishUnitSources merges the product tree and deployment materials and
// publishes every unit either one provides. Product units win when both carry
// the same name.
func publishUnitSources(product, materials string) error {
	merged, err := os.MkdirTemp("", "agent-vm-merged-units-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(merged)
	destination := filepath.Join(merged, "systemd")
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	for _, source := range []string{filepath.Join(materials, "assets/systemd"), filepath.Join(product, "systemd")} {
		for _, name := range hostUnitAssets {
			content, readErr := os.ReadFile(filepath.Join(source, name))
			if readErr != nil {
				continue
			}
			if err := os.WriteFile(filepath.Join(destination, name), content, 0o644); err != nil {
				return err
			}
		}
	}
	_, err = publishHostUnits(merged, outputUnitRoot)
	return err
}

// SameDeployBinary reports whether two deploy binaries hold identical bytes.
func SameDeployBinary(candidate, installed string) (bool, error) {
	next, err := os.ReadFile(candidate)
	if err != nil {
		return false, err
	}
	current, err := os.ReadFile(installed)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return string(next) == string(current), nil
}
