package native

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

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

// NamedLock is a process-independent lock used for lifecycle resources that
// may be acted on by both blue and green service instances. The lock file is
// keyed by a digest so caller-controlled session IDs cannot escape the lock
// directory.
type NamedLock struct {
	file *os.File
}

func AcquireNamedLock(ctx context.Context, directory, key string) (*NamedLock, error) {
	if directory == "" || key == "" {
		return nil, errors.New("named lock directory and key are required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return nil, fmt.Errorf("create named lock directory: %w", err)
	}
	digest := sha256.Sum256([]byte(key))
	path := filepath.Join(directory, "resource-"+hex.EncodeToString(digest[:])+".lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return nil, fmt.Errorf("open named lock: %w", err)
	}
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
				_ = file.Close()
				return nil, ctxErr
			}
			return &NamedLock{file: file}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = file.Close()
			return nil, fmt.Errorf("acquire named lock: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TryNamedLock is used by bounded reconciliation scans. A busy lock is a
// normal result and must leave the caller's durable operation untouched.
func TryNamedLock(directory, key string) (*NamedLock, bool, error) {
	if directory == "" || key == "" {
		return nil, false, errors.New("named lock directory and key are required")
	}
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return nil, false, fmt.Errorf("create named lock directory: %w", err)
	}
	digest := sha256.Sum256([]byte(key))
	path := filepath.Join(directory, "resource-"+hex.EncodeToString(digest[:])+".lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return nil, false, fmt.Errorf("open named lock: %w", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("try named lock: %w", err)
	}
	return &NamedLock{file: file}, true, nil
}

func (lock *NamedLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	unlockErr := unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	closeErr := lock.file.Close()
	lock.file = nil
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
