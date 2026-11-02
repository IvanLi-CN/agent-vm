package deployd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

type Client struct {
	Socket  string
	Timeout time.Duration
}

func (client Client) Call(ctx context.Context, method, path string, value any) ([]byte, int, error) {
	var body io.Reader
	if value != nil {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, 0, err
		}
		body = bytes.NewReader(encoded)
	}
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "unix", client.Socket)
	}}
	defer transport.CloseIdleConnections()
	timeout := client.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://deployd"+path, body)
	if err != nil {
		return nil, 0, err
	}
	if value != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := (&http.Client{Transport: transport, Timeout: timeout}).Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxRequestBytes+1))
	if err != nil {
		return nil, response.StatusCode, err
	}
	if len(payload) > maxRequestBytes {
		return nil, response.StatusCode, errors.New("deployd response exceeds limit")
	}
	if response.StatusCode >= 400 {
		return payload, response.StatusCode, fmt.Errorf("deployd returned HTTP %d", response.StatusCode)
	}
	return payload, response.StatusCode, nil
}
