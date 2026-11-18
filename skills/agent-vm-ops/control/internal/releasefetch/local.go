package releasefetch

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Open each component relative to a pinned directory descriptor so replacing
// a path between inspection and open cannot redirect a privileged update.
func copyProtected(source, destination string, limit int64) error {
	path, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() {
		if fd >= 0 {
			_ = unix.Close(fd)
		}
	}()
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, part := range parts {
		flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
		last := i == len(parts)-1
		if !last {
			flags |= unix.O_DIRECTORY
		}
		next, err := unix.Openat(fd, part, flags, 0)
		if err != nil {
			return fmt.Errorf("open protected release input: %w", err)
		}
		_ = unix.Close(fd)
		fd = next
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			return err
		}
		if st.Uid != 0 && st.Uid != uint32(os.Geteuid()) || st.Mode&0o022 != 0 {
			return errors.New("release input and its directories must be protected against writes by other users")
		}
		if last && (st.Mode&unix.S_IFMT != unix.S_IFREG || st.Size <= 0 || st.Size > limit) {
			return errors.New("release input must be a nonempty regular file within the size limit")
		}
	}
	file := os.NewFile(uintptr(fd), path)
	fd = -1
	defer file.Close()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(out, io.LimitReader(file, limit))
	var extra [1]byte
	_, eof := file.Read(extra[:])
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil || n == 0 || eof != io.EOF {
		_ = os.Remove(destination)
		return errors.Join(errors.New("could not stage complete release input within the size limit"), copyErr, closeErr)
	}
	return nil
}

func verifyServer(bundle, staged string) error {
	expected := digest(staged)
	file, err := os.Open(bundle)
	if err != nil {
		return err
	}
	defer file.Close()
	reader := tar.NewReader(file)
	found := false
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if header.Name != "bin/agent-vm-server" {
			continue
		}
		sum := sha256.New()
		if found || header.Typeflag != tar.TypeReg {
			return errors.New("release bundle has an invalid server artifact")
		}
		if _, err := io.Copy(sum, reader); err != nil {
			return err
		}
		if expected == "" || hex.EncodeToString(sum.Sum(nil)) != expected {
			return errors.New("specified server does not match the published bundle")
		}
		found = true
	}
	if !found {
		return errors.New("release bundle does not contain agent-vm-server")
	}
	return nil
}
