package install

import (
	"errors"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
)

// PrepareLegacySlotAccess runs before systemd starts a migrated slot. An old
// deployment daemon only opens the top-level directory after unpacking.
func PrepareLegacySlotAccess(slot string) error {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 || (slot != "blue" && slot != "green") {
		return errors.New("legacy slot access requires root and a fixed Linux slot")
	}
	group, err := user.LookupGroup("agent-vm-web")
	if err != nil {
		return err
	}
	gid, err := strconv.ParseUint(group.Gid, 10, 32)
	if err != nil {
		return err
	}
	const slots = "/var/lib/agent-vm-native/slots"
	for path := slots; ; path = filepath.Dir(path) {
		info, err := os.Lstat(path)
		if err != nil || !rootProtected(info, 0) || !info.IsDir() {
			return errors.New("legacy slot parent is not a protected root directory")
		}
		if path == "/" {
			break
		}
	}
	// Preserve root ownership while allowing the service account to traverse
	// the setgid slot root and keeping newly staged directories in the web group.
	if err := os.Lchown(slots, 0, int(gid)); err != nil {
		return err
	}
	info, err := os.Stat(slots)
	if err != nil {
		return err
	}
	if err := os.Chmod(slots, info.Mode().Perm()|os.ModeSetgid); err != nil {
		return err
	}
	return openLegacySlotDirectories(filepath.Join(slots, slot), filepath.Join("/run/agent-vm-native", slot, "app.sock"), 0, uint32(gid))
}

func rootProtected(info fs.FileInfo, uid uint32) bool {
	if info == nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uid && info.Mode()&(os.ModeSymlink|os.ModeSetuid|os.ModeSticky|0022) == 0
}

func openLegacySlotDirectories(directory, socket string, uid, gid uint32) error {
	var groupChanges, modeChanges []string
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == filepath.Join(directory, "app.sock") && entry.Type()&os.ModeSymlink != 0 {
			if target, err := os.Readlink(path); err != nil || target != socket {
				return errors.New("legacy slot socket link does not point to its own socket")
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !rootProtected(info, uid) || (!info.IsDir() && (!info.Mode().IsRegular() || info.Mode()&os.ModeSetgid != 0)) {
			return errors.New("legacy slot contains an unprotected entry")
		}
		if info.IsDir() {
			currentGID := info.Sys().(*syscall.Stat_t).Gid
			if currentGID != gid && currentGID != 0 {
				return errors.New("legacy slot directory does not belong to the slot access group")
			}
			if currentGID != gid {
				groupChanges = append(groupChanges, path)
			}
			if info.Mode().Perm() != 0750 {
				modeChanges = append(modeChanges, path)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, path := range groupChanges {
		if err := os.Lchown(path, int(uid), int(gid)); err != nil {
			return err
		}
	}
	for _, path := range modeChanges {
		if err := os.Chmod(path, 0750); err != nil {
			return err
		}
	}
	return nil
}

func transferLegacySlotDirectories(directory, socket string, deployUID int) error {
	var directories []string
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == filepath.Join(directory, "app.sock") && entry.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil || target != socket {
				return errors.New("slot socket link does not point to its own socket")
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !rootProtected(info, 0) || (!info.IsDir() && (!info.Mode().IsRegular() || info.Mode()&os.ModeSetgid != 0)) {
			return errors.New("slot tree contains an unprotected entry")
		}
		if info.IsDir() {
			directories = append(directories, path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Transfer children before the parent can be modified by the deploy user.
	for i := len(directories) - 1; i >= 0; i-- {
		if err := os.Lchown(directories[i], deployUID, -1); err != nil {
			return err
		}
	}
	return nil
}
