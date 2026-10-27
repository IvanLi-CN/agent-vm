package webadmin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	maxWebHeaderBytes   = 16 * 1024
	maxAPIResponseBytes = 4 * 1024 * 1024
)

func prepareSocket(path string, mode os.FileMode) error {
	if path == "" {
		return errors.New("socket path is empty")
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return err
	}
	if err := os.Chmod(parent, 0o750); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to replace non-socket path: %s", path)
	}
	return os.Remove(path)
}

func serveUnix(ctx context.Context, socket string, handler http.Handler) error {
	if err := prepareSocket(socket, 0o660); err != nil {
		return err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("listen on management socket: %w", err)
	}
	if err := os.Chmod(socket, 0o660); err != nil {
		_ = listener.Close()
		return err
	}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Minute,
		WriteTimeout:      15 * time.Minute,
		MaxHeaderBytes:    maxWebHeaderBytes,
		ConnContext:       connContext,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	err = server.Serve(&peerListener{Listener: listener})
	_ = listener.Close()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
