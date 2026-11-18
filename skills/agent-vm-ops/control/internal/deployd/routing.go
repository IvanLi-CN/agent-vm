package deployd

import (
	"errors"
	"os"
	"path/filepath"
)

const ManagedRoute = "/var/lib/agent-vm-native/routing/active"

// EffectiveActivePath preserves the public path used by Caddy and existing
// clients. Only the installer-owned indirection may select the managed path.
func EffectiveActivePath(public string) (string, error) {
	if public != Defaults().ActivePath {
		return public, nil
	}
	return resolveActivePath(public, ManagedRoute)
}

func resolveActivePath(public, managed string) (string, error) {
	target, err := os.Readlink(public)
	if errors.Is(err, os.ErrNotExist) {
		return public, nil
	}
	if err != nil {
		return "", err
	}
	if target == managed {
		if err := checkHostUnitDirectory(filepath.Dir(public)); err != nil {
			return "", errors.New("untrusted public routing path")
		}
		return managed, nil
	}
	return public, nil
}
