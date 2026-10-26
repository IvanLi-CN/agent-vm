package httpserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/IvanLi-CN/agent-toolkit/skills/agent-vm-ops/control/internal/lifecycle"
	"github.com/IvanLi-CN/agent-toolkit/skills/agent-vm-ops/control/internal/observe"
	"github.com/IvanLi-CN/agent-toolkit/skills/agent-vm-ops/control/internal/protocol"
)

const (
	maxHeaderBytes = 8 * 1024
	maxBodyBytes   = 2 * 1024 * 1024
)

type peerConn struct {
	net.Conn
	uid int
}

type peerListener struct{ net.Listener }

func (listener *peerListener) Accept() (net.Conn, error) {
	conn, err := listener.Listener.Accept()
	if err != nil {
		return nil, err
	}
	uid, uidErr := peerUID(conn)
	if uidErr != nil {
		_ = conn.Close()
		return nil, uidErr
	}
	return &peerConn{Conn: conn, uid: uid}, nil
}

type peerContextKey struct{}

type Server struct {
	Service   *lifecycle.Service
	Snapshots *observe.SnapshotStore
}

func New(service *lifecycle.Service, snapshots ...*observe.SnapshotStore) *Server {
	var store *observe.SnapshotStore
	if len(snapshots) > 0 {
		store = snapshots[0]
	}
	return &Server{Service: service, Snapshots: store}
}

func (s *Server) Handler() http.Handler {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery(), func(c *gin.Context) {
		if conn, ok := c.Request.Context().Value(peerContextKey{}).(*peerConn); ok {
			uid := os.Getuid()
			if (uid == 0 && conn.uid != 0) || (uid != 0 && conn.uid != uid) {
				c.AbortWithStatusJSON(http.StatusForbidden, map[string]any{"ok": false, "error": map[string]any{"code": "peer_unauthorized", "message": "Unix peer credentials are not authorized"}})
				return
			}
		}
		c.Next()
	})
	router.POST("/internal/v1/control", s.control)
	router.GET("/internal/v1/health", s.health)
	router.GET("/internal/v1/snapshots/:source", s.snapshot)
	return router
}

func (s *Server) control(c *gin.Context) {
	reader := http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
	defer c.Request.Body.Close()
	body, err := io.ReadAll(reader)
	if err != nil {
		writeError(c, protocol.NewError("request_too_large", "control request body exceeds the limit", nil), nil)
		return
	}
	request, err := protocol.Decode(body)
	if err != nil {
		writeError(c, err, nil)
		return
	}
	response, dispatchErr := s.Service.Dispatch(c.Request.Context(), request)
	if dispatchErr != nil {
		writeError(c, dispatchErr, request)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (s *Server) health(c *gin.Context) {
	ready := s.Service.Ready()
	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	c.JSON(status, map[string]any{"ok": true, "ready": ready, "protocol": protocol.Version})
}

func (s *Server) snapshot(c *gin.Context) {
	source := c.Param("source")
	if source != "capacity" && source != "inventory" && source != "health" {
		c.JSON(http.StatusNotFound, map[string]any{"ok": false, "error": map[string]any{"code": "unknown_snapshot", "message": source}})
		return
	}
	if s.Snapshots == nil {
		c.JSON(http.StatusServiceUnavailable, map[string]any{"ok": false, "error": map[string]any{"code": "snapshot_unavailable", "message": "snapshot store is not configured"}})
		return
	}
	payload, err := s.Snapshots.Read(source)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, map[string]any{"ok": false, "error": map[string]any{"code": "snapshot_unavailable", "message": "snapshot is not ready"}})
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", payload)
}

func writeError(c *gin.Context, err error, request protocol.Request) {
	control := protocol.AsError(err)
	status := http.StatusInternalServerError
	switch control.Code {
	case "invalid_request", "protocol_mismatch", "unknown_action", "missing_session", "cache_stale":
		status = http.StatusBadRequest
	case "draining", "operation_conflict", "operation_incomplete":
		status = http.StatusConflict
	case "startup_reconcile_required", "state_unverified":
		status = http.StatusServiceUnavailable
	}
	if request == nil {
		c.JSON(status, map[string]any{"ok": false, "protocol": protocol.Version, "error": control})
		return
	}
	c.JSON(status, protocol.ErrorEnvelope(request, control))
}

func Serve(ctx context.Context, socket string, service *lifecycle.Service, snapshots ...*observe.SnapshotStore) error {
	if err := prepareSocket(socket); err != nil {
		return err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("listen on control socket: %w", err)
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		_ = listener.Close()
		return err
	}
	wrapped := &peerListener{Listener: listener}
	server := &http.Server{Handler: New(service, snapshots...).Handler(), ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 600 * time.Second, ReadTimeout: 600 * time.Second, MaxHeaderBytes: maxHeaderBytes, ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
		if peer, ok := conn.(*peerConn); ok {
			return context.WithValue(ctx, peerContextKey{}, peer)
		}
		return ctx
	}}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	err = server.Serve(wrapped)
	_ = listener.Close()
	return err
}

func prepareSocket(path string) error {
	if path == "" {
		return errors.New("control socket path is empty")
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(parent, 0o700); err != nil {
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
		return fmt.Errorf("refusing to replace non-socket control path: %s", path)
	}
	return os.Remove(path)
}

func Request(ctx context.Context, socket string, body []byte) ([]byte, int, error) {
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dial net.Dialer
		return dial.DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: contextTimeout(ctx), CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/internal/v1/control", bytesReader(body))
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxBodyBytes+1))
	if err != nil {
		return nil, response.StatusCode, err
	}
	if len(payload) > maxBodyBytes {
		return nil, response.StatusCode, errors.New("control response exceeds the limit")
	}
	return payload, response.StatusCode, nil
}

func contextTimeout(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return time.Nanosecond
	}
	return remaining
}

func bytesReader(value []byte) io.Reader { return &byteReader{value: value} }

type byteReader struct {
	value  []byte
	offset int
}

func (reader *byteReader) Read(target []byte) (int, error) {
	if reader.offset >= len(reader.value) {
		return 0, io.EOF
	}
	count := copy(target, reader.value[reader.offset:])
	reader.offset += count
	return count, nil
}
