package install

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// The incumbent is stopped and the deployment singleton is held. Keep the
// existing tree and metadata so the next process reconciles the same points.
func transferRecoveryOwnership(ctx context.Context, directory string, uid, gid int) error {
	if err := safeParents(filepath.Dir(directory)); err != nil {
		return err
	}
	if err := os.Mkdir(directory, 0700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	type entry struct {
		path string
		info fs.FileInfo
	}
	var entries []entry
	err := filepath.WalkDir(directory, func(path string, item fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		// Only the material this plane restores moves to the deployment
		// account. Historical backups parked beside it (previous binaries,
		// quarantined stages, input copies) are dead weight: asserting on them
		// makes an upgrade fail because of files no transaction will read.
		if item.IsDir() && path != directory && !recoveryScopeDir(directory, path) {
			return filepath.SkipDir
		}
		if !item.IsDir() && path != directory && !recoveryScopeFile(directory, path) {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := item.Info()
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		private := info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky|0077) == 0
		if !ok || !private || (stat.Uid != 0 && (stat.Uid != uint32(uid) || stat.Gid != uint32(gid))) ||
			(!info.IsDir() && (path == directory || !info.Mode().IsRegular() || stat.Nlink != 1)) {
			return fmt.Errorf("unexpected ownership, permissions or file type: %s", path)
		}
		entries = append(entries, entry{path, info})
		return nil
	})
	if err != nil {
		return err
	}
	// Give the dedicated account access to the private root last.
	for i := len(entries) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return err
		}
		current := entries[i]
		if err := chownRecoveryEntry(current.path, current.info, uid, gid); err != nil {
			return fmt.Errorf("%s: %w", current.path, err)
		}
	}
	return syncDir(filepath.Dir(directory))
}

// recoveryScopeDir reports whether a subdirectory under the recovery root
// belongs to the deployment plane.
func recoveryScopeDir(directory, path string) bool {
	relative, err := filepath.Rel(directory, path)
	if err != nil {
		return false
	}
	return relative == "rqlite-recovery"
}

// recoveryScopeFile reports whether a file directly under the recovery root is
// a migration snapshot or a recovery receipt/pruning record.
func recoveryScopeFile(directory, path string) bool {
	relative, err := filepath.Rel(directory, path)
	if err != nil {
		return false
	}
	base := filepath.Base(relative)
	return strings.HasPrefix(base, "migration-") && strings.HasSuffix(base, ".sqlite3")
}

func chownRecoveryEntry(path string, expected fs.FileInfo, uid, gid int) error {
	flags := syscall.O_NOFOLLOW | syscall.O_CLOEXEC | syscall.O_RDONLY | syscall.O_NONBLOCK
	if expected.IsDir() {
		flags |= syscall.O_DIRECTORY
	}
	fd, err := syscall.Open(path, flags, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	stat := info.Sys().(*syscall.Stat_t)
	if !os.SameFile(expected, info) || info.Mode() != expected.Mode() ||
		(info.Mode().IsRegular() && stat.Nlink != 1) {
		return errors.New("recovery material changed during handoff")
	}
	if stat.Uid == uint32(uid) && stat.Gid == uint32(gid) {
		return nil
	}
	if err := file.Chown(uid, gid); err != nil {
		return err
	}
	return file.Sync()
}
