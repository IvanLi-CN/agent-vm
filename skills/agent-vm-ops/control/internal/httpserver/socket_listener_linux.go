//go:build linux

package httpserver

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

const unixSocketBacklog = 128

var unixSocketTempSequence atomic.Uint64

// listenUnixSocket keeps the parent directory anchored by file descriptor
// before binding. This prevents an ancestor replacement from redirecting the
// pathname between validation and bind.
func listenUnixSocket(path string, mode os.FileMode) (net.Listener, error) {
	parentFD, name, err := openUnixSocketParent(path)
	if err != nil {
		return nil, err
	}
	defer unix.Close(parentFD)
	unlockParent, err := lockUnixSocketParent(parentFD)
	if err != nil {
		return nil, err
	}
	parentLocked := true
	defer func() {
		if parentLocked {
			_ = unlockParent()
		}
	}()

	socketFD, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("create control socket: %w", err)
	}
	closeSocket := true
	defer func() {
		if closeSocket {
			_ = unix.Close(socketFD)
		}
	}()

	temporaryName, err := bindTemporaryUnixSocket(parentFD, socketFD, mode)
	if err != nil {
		return nil, fmt.Errorf("bind control socket: %w", err)
	}
	temporaryBound := true
	cleanupTemporary := func() {
		if temporaryBound {
			_ = unix.Unlinkat(parentFD, temporaryName, 0)
		}
	}
	defer cleanupTemporary()
	if err := unix.Listen(socketFD, unixSocketBacklog); err != nil {
		return nil, fmt.Errorf("listen on control socket: %w", err)
	}
	if err := chmodUnixSocketAt(parentFD, temporaryName, mode); err != nil {
		return nil, fmt.Errorf("chmod control socket: %w", err)
	}
	if err := validateUnixSocketReplacementTarget(parentFD, name); err != nil {
		return nil, err
	}
	identityFD, err := unix.Dup(socketFD)
	if err != nil {
		return nil, fmt.Errorf("duplicate control socket identity: %w", err)
	}
	defer unix.Close(identityFD)

	file := os.NewFile(uintptr(socketFD), path)
	if file == nil {
		return nil, fmt.Errorf("wrap control socket")
	}
	listener, err := net.FileListener(file)
	closeErr := file.Close()
	closeSocket = false
	if err != nil {
		return nil, fmt.Errorf("wrap control socket listener: %w", err)
	}
	if closeErr != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("close control socket descriptor: %w", closeErr)
	}
	if err := validateUnixSocketReplacementTarget(parentFD, name); err != nil {
		_ = listener.Close()
		return nil, err
	}
	if cleanup, err := installUnixSocketAt(parentFD, temporaryName, name, identityFD, mode); err != nil {
		temporaryBound = cleanup
		_ = listener.Close()
		return nil, err
	}
	temporaryBound = false
	parentLocked = false
	return &lockedUnixListener{Listener: listener, unlock: unlockParent}, nil
}

type lockedUnixListener struct {
	net.Listener
	unlock    func() error
	closeOnce sync.Once
	unlockErr error
}

func (listener *lockedUnixListener) Close() error {
	listenerErr := listener.Listener.Close()
	listener.closeOnce.Do(func() {
		listener.unlockErr = listener.unlock()
	})
	return errors.Join(listenerErr, listener.unlockErr)
}

func lockUnixSocketParent(parentFD int) (func() error, error) {
	lockFD, err := unix.Openat(parentFD, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open control socket parent lock: %w", err)
	}
	if err := unix.Flock(lockFD, unix.LOCK_EX); err != nil {
		_ = unix.Close(lockFD)
		return nil, fmt.Errorf("lock control socket parent: %w", err)
	}
	return func() error {
		unlockErr := unix.Flock(lockFD, unix.LOCK_UN)
		closeErr := unix.Close(lockFD)
		return errors.Join(unlockErr, closeErr)
	}, nil
}

func bindUnixSocket(socketFD int, path string, mode os.FileMode) error {
	// Linux applies umask when it creates the AF_UNIX pathname. This provides
	// the compatibility path for kernels without fchmodat2; the exact inode is
	// still checked after bind before the listener is exposed.
	oldUmask := unix.Umask(int(0o777 &^ uint32(mode.Perm())))
	defer unix.Umask(oldUmask)
	return unix.Bind(socketFD, &unix.SockaddrUnix{Name: path})
}

func bindTemporaryUnixSocket(parentFD, socketFD int, mode os.FileMode) (string, error) {
	for attempt := 0; attempt < 32; attempt++ {
		name := ".agent-vm-" + strconv.FormatUint(unixSocketTempSequence.Add(1), 36)
		bindPath := fmt.Sprintf("/proc/self/fd/%d/%s", parentFD, name)
		if err := bindUnixSocket(socketFD, bindPath, mode); err != nil {
			if errors.Is(err, unix.EADDRINUSE) || errors.Is(err, unix.EEXIST) {
				continue
			}
			return "", err
		}
		return name, nil
	}
	return "", errors.New("could not allocate a temporary control socket name")
}

func openUnixSocketParent(path string) (int, string, error) {
	if !filepath.IsAbs(path) {
		return -1, "", fmt.Errorf("control socket path must be absolute: %s", path)
	}
	clean := filepath.Clean(path)
	name := filepath.Base(clean)
	parent := filepath.Dir(clean)
	if name == "." || name == string(filepath.Separator) || strings.Contains(name, string(filepath.Separator)) {
		return -1, "", fmt.Errorf("control socket path has no valid leaf: %s", path)
	}

	flags := unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC
	fd, err := unix.Open("/", flags, 0)
	if err != nil {
		return -1, "", fmt.Errorf("open control socket root: %w", err)
	}
	components := strings.Split(strings.TrimPrefix(parent, string(filepath.Separator)), string(filepath.Separator))
	for index, component := range components {
		if component == "" {
			continue
		}
		next, openErr := unix.Openat(fd, component, flags|unix.O_NOFOLLOW, 0)
		if openErr == unix.ENOENT {
			if mkdirErr := unix.Mkdirat(fd, component, 0o750); mkdirErr != nil && mkdirErr != unix.EEXIST {
				_ = unix.Close(fd)
				return -1, "", fmt.Errorf("create control socket parent %s: %w", parent, mkdirErr)
			}
			next, openErr = unix.Openat(fd, component, flags|unix.O_NOFOLLOW, 0)
		}
		if openErr != nil {
			_ = unix.Close(fd)
			return -1, "", fmt.Errorf("open control socket parent %s: %w", parent, openErr)
		}
		var stat unix.Stat_t
		if statErr := unix.Fstat(next, &stat); statErr != nil {
			_ = unix.Close(next)
			_ = unix.Close(fd)
			return -1, "", fmt.Errorf("inspect control socket parent %s: %w", parent, statErr)
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
			_ = unix.Close(next)
			_ = unix.Close(fd)
			return -1, "", fmt.Errorf("control socket parent is not a directory: %s", parent)
		}
		if index == len(components)-1 {
			if chmodErr := chmodUnixSocketParent(next); chmodErr != nil {
				_ = unix.Close(next)
				_ = unix.Close(fd)
				return -1, "", fmt.Errorf("chmod control socket parent: %w", chmodErr)
			}
		}
		_ = unix.Close(fd)
		fd = next
	}
	return fd, name, nil
}

func chmodUnixSocketParent(directoryFD int) error {
	chmodFD, err := unix.Openat(directoryFD, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(chmodFD)
	return unix.Fchmod(chmodFD, 0o750)
}

func chmodUnixSocketAt(parentFD int, name string, mode os.FileMode) error {
	pathFD, err := unix.Openat(parentFD, name, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open bound control socket: %w", err)
	}
	defer unix.Close(pathFD)

	var before unix.Stat_t
	if err := unix.Fstat(pathFD, &before); err != nil {
		return fmt.Errorf("inspect bound control socket: %w", err)
	}
	if before.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return fmt.Errorf("bound control path is not a socket: %s", name)
	}
	wantedMode := uint32(mode.Perm())
	chmodErr := unix.Fchmodat(pathFD, "", wantedMode, unix.AT_EMPTY_PATH)
	if chmodErr != nil {
		if !errors.Is(chmodErr, unix.EOPNOTSUPP) && !errors.Is(chmodErr, unix.ENOSYS) {
			return fmt.Errorf("set bound control socket mode: %w", chmodErr)
		}
		// fchmodat2, which implements AT_EMPTY_PATH, was added in Linux 6.6.
		// Older kernels already applied the requested mode during bind via the
		// temporary umask above, so only accept the fallback when it is exact.
		if uint32(before.Mode)&0o7777 != wantedMode {
			return fmt.Errorf("bound control socket mode cannot be verified on this kernel: %w", chmodErr)
		}
	}

	var after unix.Stat_t
	if err := unix.Fstat(pathFD, &after); err != nil {
		return fmt.Errorf("recheck bound control socket descriptor: %w", err)
	}
	if after.Dev != before.Dev || after.Ino != before.Ino {
		return fmt.Errorf("bound control socket changed while setting mode")
	}
	if uint32(after.Mode)&0o7777 != wantedMode {
		return fmt.Errorf("bound control socket mode is %o, want %o", uint32(after.Mode)&0o7777, wantedMode)
	}
	var pathAfter unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &pathAfter, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("recheck bound control socket path: %w", err)
	}
	if pathAfter.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return fmt.Errorf("bound control path is no longer a socket: %s", name)
	}
	if pathAfter.Dev != before.Dev || pathAfter.Ino != before.Ino {
		return fmt.Errorf("bound control socket path changed while setting mode")
	}
	if uint32(pathAfter.Mode)&0o7777 != wantedMode {
		return fmt.Errorf("bound control socket path mode is %o, want %o", uint32(pathAfter.Mode)&0o7777, wantedMode)
	}
	return nil
}

func unixSocketTargetExists(parentFD int, name string) (bool, error) {
	var stat unix.Stat_t
	err := unix.Fstatat(parentFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		if err == unix.ENOENT {
			return false, nil
		}
		return false, fmt.Errorf("inspect control socket: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return false, fmt.Errorf("refusing to replace non-socket control path: %s", name)
	}
	return true, nil
}

func validateUnixSocketReplacementTarget(parentFD int, name string) error {
	_, err := unixSocketTargetExists(parentFD, name)
	return err
}

func verifyUnixSocketAt(parentFD int, name string, identityFD int, mode os.FileMode) error {
	var identity unix.Stat_t
	if err := unix.Fstat(identityFD, &identity); err != nil {
		return fmt.Errorf("inspect prepared control socket: %w", err)
	}
	if identity.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return fmt.Errorf("prepared control path is not a socket: %s", name)
	}
	var pathStat unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &pathStat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("recheck installed control socket path: %w", err)
	}
	if pathStat.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return fmt.Errorf("installed control path is not a socket: %s", name)
	}
	if pathStat.Dev != identity.Dev || pathStat.Ino != identity.Ino {
		return fmt.Errorf("installed control socket path changed while replacing")
	}
	wantedMode := uint32(mode.Perm())
	if uint32(pathStat.Mode)&0o7777 != wantedMode {
		return fmt.Errorf("installed control socket path mode is %o, want %o", uint32(pathStat.Mode)&0o7777, wantedMode)
	}
	return nil
}

func unlinkUnixSocketIfSame(parentFD int, name string, identityFD int) error {
	var pathStat unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &pathStat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if err == unix.ENOENT {
			return nil
		}
		return err
	}
	var identity unix.Stat_t
	if err := unix.Fstat(identityFD, &identity); err != nil {
		return err
	}
	if pathStat.Dev != identity.Dev || pathStat.Ino != identity.Ino {
		return errors.New("control socket path identity changed while cleaning up")
	}
	return unix.Unlinkat(parentFD, name, 0)
}

// installUnixSocketAt swaps a fully prepared socket into place. The boolean
// reports whether the temporary name still needs cleanup after an error.
func installUnixSocketAt(parentFD int, temporaryName, name string, identityFD int, mode os.FileMode) (bool, error) {
	targetExists, err := unixSocketTargetExists(parentFD, name)
	if err != nil {
		return true, err
	}
	if targetExists {
		if err := unix.Renameat2(parentFD, temporaryName, parentFD, name, unix.RENAME_EXCHANGE); err != nil {
			return true, fmt.Errorf("exchange control socket: %w", err)
		}
	} else if err := unix.Renameat2(parentFD, temporaryName, parentFD, name, unix.RENAME_NOREPLACE); err != nil {
		return true, fmt.Errorf("install control socket: %w", err)
	}

	if err := verifyUnixSocketAt(parentFD, name, identityFD, mode); err != nil {
		if targetExists {
			rollbackErr := unix.Renameat2(parentFD, name, parentFD, temporaryName, unix.RENAME_EXCHANGE)
			if rollbackErr == nil {
				return true, err
			}
			return false, errors.Join(err, fmt.Errorf("restore previous control socket: %w", rollbackErr))
		}
		cleanupErr := unlinkUnixSocketIfSame(parentFD, name, identityFD)
		if cleanupErr == nil {
			return true, err
		}
		return false, errors.Join(err, fmt.Errorf("remove invalid installed control socket: %w", cleanupErr))
	}

	if targetExists {
		if err := unix.Unlinkat(parentFD, temporaryName, 0); err != nil {
			rollbackErr := unix.Renameat2(parentFD, name, parentFD, temporaryName, unix.RENAME_EXCHANGE)
			if rollbackErr == nil {
				return true, fmt.Errorf("remove previous control socket: %w", err)
			}
			return false, errors.Join(fmt.Errorf("remove previous control socket: %w", err), fmt.Errorf("restore previous control socket: %w", rollbackErr))
		}
	}
	return false, nil
}
