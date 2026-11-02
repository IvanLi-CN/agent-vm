//go:build linux

package httpserver

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const unixSocketBacklog = 128

// listenUnixSocket keeps the parent directory anchored by file descriptor
// before binding. This prevents an ancestor replacement from redirecting the
// pathname between validation and bind.
func listenUnixSocket(path string, mode os.FileMode) (net.Listener, error) {
	if err := prepareUnixSocketParent(path); err != nil {
		return nil, err
	}
	parentFD, name, err := openUnixSocketParent(path)
	if err != nil {
		return nil, err
	}
	defer unix.Close(parentFD)

	if err := prepareUnixSocketAt(parentFD, name); err != nil {
		return nil, err
	}
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

	bindPath := fmt.Sprintf("/proc/self/fd/%d/%s", parentFD, name)
	if err := unix.Bind(socketFD, &unix.SockaddrUnix{Name: bindPath}); err != nil {
		return nil, fmt.Errorf("bind control socket: %w", err)
	}
	if err := unix.Listen(socketFD, unixSocketBacklog); err != nil {
		return nil, fmt.Errorf("listen on control socket: %w", err)
	}
	if err := unix.Fchmodat(parentFD, name, uint32(mode.Perm()), 0); err != nil {
		return nil, fmt.Errorf("chmod control socket: %w", err)
	}

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
	return listener, nil
}

func prepareUnixSocketParent(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("control socket path must be absolute: %s", path)
	}
	parent := filepath.Dir(filepath.Clean(path))
	if parent == string(filepath.Separator) {
		return nil
	}

	flags := unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC
	fd, err := unix.Open("/", flags, 0)
	if err != nil {
		return fmt.Errorf("open control socket root: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()

	components := strings.Split(strings.TrimPrefix(parent, string(filepath.Separator)), string(filepath.Separator))
	for _, component := range components {
		if component == "" {
			continue
		}
		next, openErr := unix.Openat(fd, component, flags|unix.O_NOFOLLOW, 0)
		if openErr == unix.ENOENT {
			if mkdirErr := unix.Mkdirat(fd, component, 0o750); mkdirErr != nil && mkdirErr != unix.EEXIST {
				return fmt.Errorf("create control socket parent %s: %w", parent, mkdirErr)
			}
			next, openErr = unix.Openat(fd, component, flags|unix.O_NOFOLLOW, 0)
		}
		if openErr != nil {
			return fmt.Errorf("open control socket parent %s: %w", parent, openErr)
		}
		var stat unix.Stat_t
		if statErr := unix.Fstat(next, &stat); statErr != nil {
			_ = unix.Close(next)
			return fmt.Errorf("inspect control socket parent %s: %w", parent, statErr)
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
			_ = unix.Close(next)
			return fmt.Errorf("control socket parent is not a directory: %s", parent)
		}
		_ = unix.Close(fd)
		fd = next
	}
	if err := unix.Fchmod(fd, 0o750); err != nil {
		return fmt.Errorf("chmod control socket parent: %w", err)
	}
	return nil
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
	for _, component := range components {
		if component == "" {
			continue
		}
		next, openErr := unix.Openat(fd, component, flags|unix.O_NOFOLLOW, 0)
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
		_ = unix.Close(fd)
		fd = next
	}
	return fd, name, nil
}

func prepareUnixSocketAt(parentFD int, name string) error {
	var stat unix.Stat_t
	err := unix.Fstatat(parentFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		if err == unix.ENOENT {
			return nil
		}
		return fmt.Errorf("inspect control socket: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return fmt.Errorf("refusing to replace non-socket control path: %s", name)
	}
	if err := unix.Unlinkat(parentFD, name, 0); err != nil {
		return fmt.Errorf("remove previous control socket: %w", err)
	}
	return nil
}
