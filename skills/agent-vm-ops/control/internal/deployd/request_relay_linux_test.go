//go:build linux

package deployd

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRequestRelayAcceptsOnlyBoundedControlRequests(t *testing.T) {
	body := `{"protocol":4,"action":"status","session_id":"relay-proof"}`
	for _, tc := range []struct {
		name, method, path, body string
		length                   int
		valid                    bool
	}{
		{"status", "POST", "/internal/v1/control", body, len(body), true},
		{"role route", "POST", "/internal/v1/controller-role", body, len(body), false},
		{"query", "POST", "/internal/v1/control?slot=green", body, len(body), false},
		{"absolute URL", "POST", "http://other/internal/v1/control", body, len(body), false},
		{"method", "GET", "/internal/v1/control", body, len(body), false},
		{"malformed object", "POST", "/internal/v1/control", `{`, 1, false},
		{"truncated", "POST", "/internal/v1/control", body, len(body) + 1, false},
		{"oversize", "POST", "/internal/v1/control", strings.Repeat("x", requestRelayLimit+1), requestRelayLimit + 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := fmt.Sprintf("%s %s HTTP/1.1\r\nHost: unix\r\nContent-Length: %d\r\n\r\n%s", tc.method, tc.path, tc.length, tc.body)
			got, err := readRelayHTTPRequest(strings.NewReader(message))
			if (err == nil) != tc.valid || (tc.valid && string(got) != body) {
				t.Fatalf("body=%q err=%v valid=%v", got, err, tc.valid)
			}
		})
	}
}

func TestRequestRelayPreservesResponseAndNeverReplays(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			socket := filepath.Join(t.TempDir(), "app.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			body := []byte(`{"protocol":4,"action":"status","session_id":"relay-proof","operation_id":"keep-this-id"}`)
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				got, _ := io.ReadAll(r.Body)
				if r.Method != "POST" || r.RequestURI != "/internal/v1/control" || !bytes.Equal(got, body) {
					t.Error("forwarded request changed")
				}
				if fail {
					conn, _, _ := w.(http.Hijacker).Hijack()
					conn.Close()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Agent-VM-Exit-Code", "2")
				w.WriteHeader(409)
				_, _ = io.WriteString(w, `{"ok":false,"protocol":4,"error":{"code":"generation_conflict"}}`)
			})}
			go server.Serve(listener)
			defer server.Close()
			var output bytes.Buffer
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := forwardRequestRelay(ctx, socket, body, &output); err != nil {
				t.Fatal(err)
			}
			response, err := http.ReadResponse(bufio.NewReader(&output), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			payload, _ := io.ReadAll(response.Body)
			if calls.Load() != 1 {
				t.Fatal("request was replayed")
			}
			if !fail && (response.StatusCode != 409 || response.Header.Get("X-Agent-VM-Exit-Code") != "2" || !bytes.Contains(payload, []byte("generation_conflict"))) {
				t.Fatal("backend response changed")
			}
			if fail && (response.StatusCode != 503 || response.Header.Get("X-Agent-VM-Exit-Code") != "3" || !bytes.Contains(payload, []byte("uncertain"))) {
				t.Fatal("uncertain transport failure was hidden")
			}
		})
	}
}
