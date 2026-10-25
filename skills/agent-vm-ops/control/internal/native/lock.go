package native

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type AuthorityLock struct {
	file *os.File
}

func AcquireLock(path string, exclusive bool, nonBlocking bool) (*AuthorityLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return nil, err
	}
	flags := unix.LOCK_SH
	if exclusive {
		flags = unix.LOCK_EX
	}
	if nonBlocking {
		flags |= unix.LOCK_NB
	}
	if err := unix.Flock(int(file.Fd()), flags); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("authority lock: %w", err)
	}
	return &AuthorityLock{file: file}, nil
}

func (lock *AuthorityLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	_ = unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	err := lock.file.Close()
	lock.file = nil
	return err
}
