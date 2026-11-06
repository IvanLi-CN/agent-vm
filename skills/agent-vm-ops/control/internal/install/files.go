package install

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
)

// installFile never replaces an existing file. An equal owned regular file is
// a verified idempotent result; a conflict preserves the operator's file.
func installFile(path string, data []byte, mode os.FileMode, group string) error {
	if err := safeParents(filepath.Dir(path)); err != nil {
		return err
	}
	gid := 0
	if group != "" {
		g, err := user.LookupGroup(group)
		if err != nil {
			return err
		}
		gid, err = strconv.Atoi(g.Gid)
		if err != nil {
			return err
		}
	}
	if info, err := os.Lstat(path); err == nil {
		owner, owned := info.Sys().(*syscall.Stat_t)
		if !owned || owner.Uid != 0 || int(owner.Gid) != gid || !info.Mode().IsRegular() || info.Mode().Perm() != mode {
			return fmt.Errorf("refusing to replace existing installation path: %s", path)
		}
		existing, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(existing, data) {
			return fmt.Errorf("existing installation file differs: %s", path)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Publish a fully synced inode without clobbering a concurrent installation.
	f, err := os.CreateTemp(filepath.Dir(path), ".install-")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Chown(0, gid); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Link(temp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}
func safeParents(path string) error {
	if path == "/" {
		return nil
	}
	if err := safeParents(filepath.Dir(path)); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return os.Mkdir(path, 0750)
	}
	if err != nil {
		return err
	}
	owner, owned := info.Sys().(*syscall.Stat_t)
	if !owned || owner.Uid != 0 || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("unsafe installation parent: %s", path)
	}
	return nil
}
func copyInstall(source, path string, mode os.FileMode, group string) error {
	f, err := os.Open(source)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 256<<20+1))
	if err != nil {
		return err
	}
	if len(data) > 256<<20 {
		return errors.New("installation file exceeds budget")
	}
	return installFile(path, data, mode, group)
}
func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
