package install

import (
	"errors"
	"os"
	"path/filepath"
)

func prepareFreshRoute(public, managed string) error {
	if _, err := os.Lstat(managed); !errors.Is(err, os.ErrNotExist) {
		return errors.New("fresh routing directory already contains a selection")
	}
	if err := os.Symlink(managed, public); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		if target, err := os.Readlink(public); err != nil || target != managed {
			return errors.New("existing public routing path is not the managed indirection")
		}
	}
	return syncDir(filepath.Dir(public))
}

// adoptActiveRoute is used by host maintenance while deployment writes are
// excluded. The old and new pointers resolve to the same slot through the
// whole rename, including when Caddy opens a connection during the change.
func adoptActiveRoute(public, managed, slots string) error {
	target, err := os.Readlink(public)
	if err != nil {
		return err
	}
	if target == managed {
		target, err = os.Readlink(managed)
		if err != nil {
			return err
		}
		return validateSlotRoute(target, slots)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(public), target)
	}
	if err := validateSlotRoute(target, slots); err != nil {
		return err
	}
	if err := os.Symlink(target, managed); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		if existing, err := os.Readlink(managed); err != nil || existing != target {
			return errors.New("managed route disagrees with the active slot")
		}
	}
	if err := syncDir(filepath.Dir(managed)); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(filepath.Dir(public), ".route-")
	if err != nil {
		return err
	}
	defer os.Remove(dir)
	next := filepath.Join(dir, "active")
	if err := os.Symlink(managed, next); err != nil {
		return err
	}
	defer os.Remove(next)
	if err := os.Rename(next, public); err != nil {
		return err
	}
	return syncDir(filepath.Dir(public))
}

func validateSlotRoute(target, slots string) error {
	if target != filepath.Join(slots, "blue") && target != filepath.Join(slots, "green") {
		return errors.New("active route does not point to a fixed slot")
	}
	info, err := os.Lstat(target)
	if err != nil || !info.IsDir() {
		return errors.New("active route slot directory is unavailable")
	}
	return nil
}
