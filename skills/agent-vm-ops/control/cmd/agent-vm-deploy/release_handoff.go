package main

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
)

func exposeVerifiedBundle(directory, bundle string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	account, err := user.LookupGroup("agent-vm-deploy")
	if _, absent := err.(user.UnknownGroupError); absent {
		return nil // Historical installations still consume the bundle as root.
	}
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil || gid <= 0 || filepath.Dir(bundle) != directory {
		return errors.New("invalid verified deployment bundle handoff")
	}
	info, err := os.Lstat(bundle)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
		return errors.New("verified deployment bundle must be a protected regular file")
	}
	if err := os.Chown(bundle, 0, gid); err != nil {
		return err
	}
	if err := os.Chmod(bundle, 0640); err != nil {
		return err
	}
	if err := os.Chown(directory, 0, gid); err != nil {
		return err
	}
	return os.Chmod(directory, 0710)
}
