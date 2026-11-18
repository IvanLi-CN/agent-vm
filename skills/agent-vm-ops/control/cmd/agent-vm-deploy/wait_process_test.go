package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWaitProcessAcceptsFencedProcessWithoutControllerReadiness(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "avm-wait-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	listener, err := net.Listen("unix", filepath.Join(root, "s"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/internal/v1/health" {
			t.Errorf("startup helper performed an unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"ok":true,"protocol":4,"process_ready":true,"process_id":123,"process_start_time":"456","ready":false,"controller_owned":false,"traffic_accepting":false}`))
	}))
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := waitSlotProcess(ctx, listener.Addr().String(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestWaitProcessRejectsMissingProcessAndBoundsStartup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := waitSlotProcess(ctx, filepath.Join(t.TempDir(), "missing"), nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("missing process accepted or deadline lost: %v", err)
	}
	for _, args := range [][]string{{"wait-process"}, {"wait-process", "--slot", "../blue"}, {"wait-process", "--slot", "blue", "--timeout", "56"}} {
		if err := run(args); err == nil {
			t.Fatalf("invalid startup arguments accepted: %v", args)
		}
	}
	for _, args := range [][]string{{"wait-authority"}, {"wait-authority", "--slot", "../blue"}, {"wait-authority", "--slot", "blue", "--timeout", "56"}, {"wait-authority", "--slot", "green", "--timeout", "0"}} {
		if err := run(args); err == nil {
			t.Fatalf("invalid authority startup arguments accepted: %v", args)
		}
	}
}

func TestWaitProcessReturnsWhenSystemdMainProcessExits(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	exited := errors.New("main process has exited")
	calls := 0
	err := waitSlotProcess(ctx, filepath.Join(t.TempDir(), "missing"), func(context.Context) error {
		calls++
		if calls == 2 {
			return exited
		}
		return nil
	})
	if !errors.Is(err, exited) {
		t.Fatalf("startup should stop waiting for the exited process: %v", err)
	}
}
