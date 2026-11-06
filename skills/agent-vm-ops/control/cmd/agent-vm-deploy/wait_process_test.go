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
	if err := waitSlotProcess(ctx, listener.Addr().String()); err != nil {
		t.Fatal(err)
	}
}

func TestWaitProcessRejectsMissingProcessAndBoundsStartup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := waitSlotProcess(ctx, filepath.Join(t.TempDir(), "missing")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("missing process accepted or deadline lost: %v", err)
	}
	for _, args := range [][]string{{"wait-process"}, {"wait-process", "--slot", "../blue"}, {"wait-process", "--slot", "blue", "--timeout", "56"}} {
		if err := run(args); err == nil {
			t.Fatalf("invalid startup arguments accepted: %v", args)
		}
	}
}
