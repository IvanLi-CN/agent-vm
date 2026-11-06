package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// wait-process is the systemd process-start boundary, not business readiness.
// A fenced slot must finish starting before Deployd can inspect or fence it.
func runWaitProcess(args []string) error {
	flags := flag.NewFlagSet("wait-process", flag.ContinueOnError)
	slot := flags.String("slot", "", "slot process to await")
	timeout := flags.Int("timeout", 55, "process startup timeout in seconds")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || (*slot != "blue" && *slot != "green") || *timeout <= 0 || *timeout > 55 {
		return errors.New("wait-process requires --slot blue|green and --timeout 1..55")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeout)*time.Second)
	defer cancel()
	return waitSlotProcess(ctx, "/run/agent-vm-native/"+*slot+"/app.sock")
}

func waitSlotProcess(ctx context.Context, socket string) error {
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix/internal/v1/health", nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err == nil {
			var health struct {
				OK               bool   `json:"ok"`
				Protocol         int    `json:"protocol"`
				ProcessReady     bool   `json:"process_ready"`
				ProcessID        int    `json:"process_id"`
				ProcessStartTime string `json:"process_start_time"`
			}
			decodeErr := json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&health)
			response.Body.Close()
			if decodeErr == nil && (response.StatusCode == http.StatusOK || response.StatusCode == http.StatusServiceUnavailable) && health.OK && health.Protocol == 4 && health.ProcessReady && health.ProcessID > 0 && health.ProcessStartTime != "" {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("slot process did not expose a valid fenced health interface: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
