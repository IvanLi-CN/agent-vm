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
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/lifecycle"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/observe"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/protocol"
)

const (
	maxHeaderBytes = 8 * 1024
	maxBodyBytes   = 2 * 1024 * 1024
	exitCodeHeader = "X-Agent-VM-Exit-Code"
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
	App       http.Handler
}

func New(service *lifecycle.Service, snapshots ...*observe.SnapshotStore) *Server {
	var store *observe.SnapshotStore
	if len(snapshots) > 0 {
		store = snapshots[0]
	}
	return &Server{Service: service, Snapshots: store}
}

func NewWithApp(service *lifecycle.Service, snapshots *observe.SnapshotStore, app http.Handler) *Server {
	return &Server{Service: service, Snapshots: snapshots, App: app}
}

func (s *Server) Handler() http.Handler {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.CustomRecovery(func(c *gin.Context, _ any) {
		writeJSON(c, http.StatusInternalServerError, map[string]any{"ok": false, "protocol": protocol.Version, "error": protocol.NewError("internal_error", "internal control server failure", nil)}, 3)
		c.Abort()
	}), func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/internal/v1/") {
			conn, ok := c.Request.Context().Value(peerContextKey{}).(*peerConn)
			if !ok {
				c.Next()
				return
			}
			uid := os.Getuid()
			if (uid == 0 && conn.uid != 0) || (uid != 0 && conn.uid != uid) {
				writeJSON(c, http.StatusForbidden, map[string]any{"ok": false, "protocol": protocol.Version, "error": protocol.NewError("peer_unauthorized", "Unix peer credentials are not authorized", nil)}, 2)
				c.Abort()
				return
			}
		}
		c.Next()
	})
	router.POST("/internal/v1/control", s.control)
	router.GET("/internal/v1/health", s.health)
	router.GET("/internal/v1/snapshots/:source", s.snapshot)
	router.NoRoute(func(c *gin.Context) {
		if s.App != nil {
			s.App.ServeHTTP(c.Writer, c.Request)
			return
		}
		c.Status(http.StatusNotFound)
	})
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
	writeJSON(c, http.StatusOK, response, 0)
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
		writeJSON(c, http.StatusNotFound, map[string]any{"ok": false, "protocol": protocol.Version, "error": protocol.NewError("unknown_snapshot", source, nil)}, 2)
		return
	}
	if s.Snapshots == nil {
		writeJSON(c, http.StatusServiceUnavailable, map[string]any{"ok": false, "protocol": protocol.Version, "error": protocol.NewError("snapshot_unavailable", "snapshot store is not configured", nil)}, 2)
		return
	}
	payload, err := s.Snapshots.Read(source)
	if err != nil {
		writeJSON(c, http.StatusServiceUnavailable, map[string]any{"ok": false, "protocol": protocol.Version, "error": protocol.NewError("snapshot_unavailable", "snapshot is not ready", nil)}, 2)
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", payload)
}

func writeError(c *gin.Context, err error, request protocol.Request) {
	control := protocol.AsError(err)
	status := http.StatusInternalServerError
	exitCode := 2
	switch control.Code {
	case "invalid_request", "protocol_mismatch", "unknown_action", "missing_session", "cache_stale", "request_too_large", "config_invalid", "unsupported_action", "resource_config_mismatch", "compatibility_action_mismatch", "compatibility_invalid", "request_schema_incompatible", "request_semantic_incompatible", "response_schema_incompatible", "response_semantic_incompatible":
		status = http.StatusBadRequest
		if control.Code == "request_too_large" {
			status = http.StatusRequestEntityTooLarge
		}
	case "draining", "operation_conflict", "operation_incomplete", "generation_mismatch", "lease_expired", "task_missing", "task_not_active", "task_transition", "cache_ineligible", "resource_insufficient":
		status = http.StatusConflict
	case "startup_reconcile_required", "state_unverified", "sqlite_unavailable", "observation_unavailable", "resource_unverified", "resource_pressure", "mapping_unverified", "ownership_mismatch", "guest_host_key_missing":
		status = http.StatusServiceUnavailable
	case "internal_error":
		exitCode = 3
	}
	if status == http.StatusInternalServerError && control.Code == "internal_error" {
		exitCode = 3
	}
	if request == nil {
		writeJSON(c, status, map[string]any{"ok": false, "protocol": protocol.Version, "error": control}, exitCode)
		return
	}
	writeJSON(c, status, protocol.ErrorEnvelope(request, control), exitCode)
}

func writeJSON(c *gin.Context, status int, value any, exitCode int) {
	c.Header(exitCodeHeader, strconv.Itoa(exitCode))
	c.JSON(status, value)
}

func Serve(ctx context.Context, socket string, service *lifecycle.Service, snapshots ...*observe.SnapshotStore) error {
	var store *observe.SnapshotStore
	if len(snapshots) > 0 {
		store = snapshots[0]
	}
	return serve(ctx, socket, service, store, nil)
}

func ServeApp(ctx context.Context, socket string, service *lifecycle.Service, snapshots *observe.SnapshotStore, app http.Handler) error {
	return serve(ctx, socket, service, snapshots, app)
}

func serve(ctx context.Context, socket string, service *lifecycle.Service, snapshots *observe.SnapshotStore, app http.Handler) error {
	if err := prepareSocket(socket); err != nil {
		return err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("listen on control socket: %w", err)
	}
	mode := os.FileMode(0o600)
	if app != nil {
		mode = 0o660
	}
	if err := os.Chmod(socket, mode); err != nil {
		_ = listener.Close()
		return err
	}
	wrapped := &peerListener{Listener: listener}
	server := &http.Server{Handler: NewWithApp(service, snapshots, app).Handler(), ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 600 * time.Second, ReadTimeout: 600 * time.Second, MaxHeaderBytes: maxHeaderBytes, ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
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
		return fmt.Errorf("refusing to replace non-socket control path: %s", path)
	}
	return os.Remove(path)
}

func Request(ctx context.Context, socket string, body []byte) ([]byte, int, error) {
	payload, status, _, err := RequestWithExitCode(ctx, socket, body)
	return payload, status, err
}

// RequestWithExitCode validates the fixed control response before the caller
// exposes it as the legacy stdout/stderr protocol.
func RequestWithExitCode(ctx context.Context, socket string, body []byte) ([]byte, int, int, error) {
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dial net.Dialer
		return dial.DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: contextTimeout(ctx), CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/internal/v1/control", bytesReader(body))
	if err != nil {
		return nil, 0, 3, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, 3, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxBodyBytes+1))
	if err != nil {
		return nil, response.StatusCode, 3, err
	}
	if len(payload) > maxBodyBytes {
		return nil, response.StatusCode, 3, errors.New("control response exceeds the limit")
	}
	contentType := response.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.ToLower(contentType), "application/json") {
		return nil, response.StatusCode, 3, errors.New("control response has an invalid content type")
	}
	exitCode, parseErr := strconv.Atoi(response.Header.Get(exitCodeHeader))
	if parseErr != nil || exitCode < 0 || exitCode > 3 || exitCode == 1 {
		return nil, response.StatusCode, 3, errors.New("control response has an invalid exit-code header")
	}
	value, decodeErr := protocol.DecodeValue(payload)
	if decodeErr != nil {
		return nil, response.StatusCode, 3, decodeErr
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, response.StatusCode, 3, errors.New("control response is not an object")
	}
	okValue, hasOK := object["ok"].(bool)
	if exitCode == 0 {
		if response.StatusCode != http.StatusOK || !hasOK || !okValue {
			return nil, response.StatusCode, 3, errors.New("successful control response is inconsistent")
		}
	} else {
		if response.StatusCode < 400 || !hasOK || okValue {
			return nil, response.StatusCode, 3, errors.New("error control response is inconsistent")
		}
		if _, hasError := object["error"].(map[string]any); !hasError {
			return nil, response.StatusCode, 3, errors.New("error control response is missing its envelope")
		}
	}
	return payload, response.StatusCode, exitCode, nil
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
