package deployd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const maxRequestBytes = 2 * 1024 * 1024

type Server struct {
	Manager *Manager
	Metrics *Metrics

	mu        sync.RWMutex
	latest    RuntimeSnapshot
	startedAt time.Time
}

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

type metricsACLListener struct {
	net.Listener
	networks []*net.IPNet
}

func (listener *metricsACLListener) Accept() (net.Conn, error) {
	for {
		conn, err := listener.Listener.Accept()
		if err != nil {
			return nil, err
		}
		address, ok := conn.RemoteAddr().(*net.TCPAddr)
		if ok && networkAllowed(address.IP, listener.networks) {
			return conn, nil
		}
		_ = conn.Close()
	}
}

func NewServer(manager *Manager, metrics *Metrics) *Server {
	if metrics == nil {
		metrics = NewMetrics(manager)
	}
	return &Server{Manager: manager, Metrics: metrics, startedAt: time.Now().UTC()}
}

func (server *Server) Handler() http.Handler {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.CustomRecovery(func(c *gin.Context, _ any) {
		writeError(c, http.StatusInternalServerError, "internal_error", "deployd request failed")
	}), func(c *gin.Context) {
		peer, ok := c.Request.Context().Value(peerContextKey{}).(*peerConn)
		if !ok {
			c.Next()
			return
		}
		uid := os.Getuid()
		if (uid == 0 && peer.uid != 0) || (uid != 0 && peer.uid != uid) {
			writeError(c, http.StatusForbidden, "peer_unauthorized", "Unix peer credentials are not authorized")
			c.Abort()
			return
		}
		c.Next()
	})
	router.GET("/v1/status", server.status)
	router.GET("/v1/transactions/:id", server.transaction)
	router.POST("/v1/plan", server.plan)
	router.POST("/v1/verify", server.verify)
	router.POST("/v1/stage", server.stage)
	router.POST("/v1/activate", server.activate)
	router.POST("/v1/rollback", server.rollback)
	router.POST("/v1/recover", server.recover)
	router.NoRoute(func(c *gin.Context) {
		writeError(c, http.StatusNotFound, "not_found", "deployd endpoint was not found")
	})
	return router
}

func (server *Server) MetricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		server.mu.RLock()
		metrics := server.Metrics.Render()
		server.mu.RUnlock()
		writer.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer, metrics)
	})
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) { writer.WriteHeader(http.StatusNotFound) })
	return mux
}

func (server *Server) Sample(ctx context.Context) error {
	status, err := server.Manager.Status(ctx)
	if err != nil {
		return err
	}
	server.mu.Lock()
	server.latest = status.Observed
	server.Metrics.Set(status.Observed, status.Transactions)
	server.mu.Unlock()
	return nil
}

func (server *Server) status(c *gin.Context) {
	status, err := server.Manager.Status(c.Request.Context())
	if err != nil {
		writeError(c, http.StatusServiceUnavailable, "observation_unavailable", err.Error())
		return
	}
	writeOK(c, status)
}

func (server *Server) transaction(c *gin.Context) {
	value, err := server.Manager.Transaction(c.Request.Context(), c.Param("id"))
	if err != nil {
		writeError(c, http.StatusNotFound, "transaction_not_found", err.Error())
		return
	}
	writeOK(c, value)
}

type request struct {
	TransactionID string `json:"transaction_id"`
	TargetSlot    string `json:"target_slot"`
	Bundle        string `json:"bundle"`
	Action        string `json:"action"`
}

func (server *Server) plan(c *gin.Context) {
	var value request
	if !decodeRequest(c, &value) {
		return
	}
	result, err := server.Manager.Plan(c.Request.Context(), value.TargetSlot, value.Bundle)
	if err != nil {
		writeError(c, http.StatusConflict, "plan_rejected", err.Error())
		return
	}
	writeOK(c, result)
}

func (server *Server) stage(c *gin.Context) {
	var value request
	if !decodeRequest(c, &value) {
		return
	}
	transaction, err := server.Manager.Stage(c.Request.Context(), value.TargetSlot, value.Bundle)
	if err != nil {
		writeError(c, http.StatusConflict, "stage_rejected", err.Error())
		return
	}
	writeOK(c, transaction)
}

func (server *Server) verify(c *gin.Context) {
	var value request
	if !decodeRequest(c, &value) {
		return
	}
	transaction, err := server.Manager.Verify(c.Request.Context(), value.TransactionID)
	if err != nil {
		writeError(c, http.StatusConflict, "verify_rejected", err.Error())
		return
	}
	writeOK(c, transaction)
}

func (server *Server) activate(c *gin.Context) {
	var value request
	if !decodeRequest(c, &value) {
		return
	}
	transaction, err := server.Manager.Activate(c.Request.Context(), value.TransactionID)
	if err != nil {
		if errors.Is(err, ErrUncertain) {
			writeError(c, http.StatusConflict, "state_unverified", err.Error())
			return
		}
		writeError(c, http.StatusConflict, "activate_rejected", err.Error())
		return
	}
	writeOK(c, transaction)
}

func (server *Server) rollback(c *gin.Context) {
	var value request
	if !decodeRequest(c, &value) {
		return
	}
	transaction, err := server.Manager.Rollback(c.Request.Context(), value.TransactionID)
	if err != nil {
		writeError(c, http.StatusConflict, "rollback_rejected", err.Error())
		return
	}
	writeOK(c, transaction)
}

func (server *Server) recover(c *gin.Context) {
	var value request
	if !decodeRequest(c, &value) {
		return
	}
	transaction, err := server.Manager.Recover(c.Request.Context(), value.TransactionID, value.Action)
	if err != nil {
		writeError(c, http.StatusConflict, "recover_rejected", err.Error())
		return
	}
	writeOK(c, transaction)
}

func decodeRequest(c *gin.Context, target any) bool {
	reader := http.MaxBytesReader(c.Writer, c.Request.Body, maxRequestBytes)
	defer c.Request.Body.Close()
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(c, http.StatusBadRequest, "invalid_request", "request body is invalid")
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		writeError(c, http.StatusBadRequest, "invalid_request", "request body contains trailing JSON")
		return false
	}
	return true
}

func writeOK(c *gin.Context, value any) {
	c.JSON(http.StatusOK, map[string]any{"ok": true, "value": value})
}

func writeError(c *gin.Context, status int, code, message string) {
	c.JSON(status, map[string]any{"ok": false, "error": map[string]any{"code": code, "message": message}})
}

func ServeUDS(ctx context.Context, socket string, handler http.Handler) error {
	if socket == "" {
		return errors.New("deployd socket path is empty")
	}
	if err := os.MkdirAll(filepathDir(socket), 0o750); err != nil {
		return err
	}
	if info, err := os.Lstat(socket); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
			return errors.New("deployd socket path is not a socket")
		}
		if err := os.Remove(socket); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	if err := os.Chmod(socket, 0o660); err != nil {
		_ = listener.Close()
		return err
	}
	wrapped := &peerListener{Listener: listener}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, MaxHeaderBytes: 8 * 1024, ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
		if peer, ok := conn.(*peerConn); ok {
			return context.WithValue(ctx, peerContextKey{}, peer)
		}
		return ctx
	}}
	go shutdownOnCancel(ctx, server)
	err = server.Serve(wrapped)
	_ = listener.Close()
	return err
}

func ServeMetrics(ctx context.Context, address string, handler http.Handler, tlsConfig MetricsTLSConfig, networkACL ...string) error {
	if MetricsAddressRequiresTLS(address) && (tlsConfig.Certificate == "" || tlsConfig.PrivateKey == "" || tlsConfig.ClientCA == "") {
		return errors.New("non-loopback metrics listener requires mTLS")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	if MetricsAddressRequiresTLS(address) {
		acl := ""
		if len(networkACL) > 0 {
			acl = networkACL[0]
		}
		networks, err := parseNetworkACL(acl)
		if err != nil {
			_ = listener.Close()
			return fmt.Errorf("metrics network ACL is invalid: %w", err)
		}
		listener = &metricsACLListener{Listener: listener, networks: networks}
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}
	if tlsConfig.Certificate != "" {
		certificate, err := tls.LoadX509KeyPair(tlsConfig.Certificate, tlsConfig.PrivateKey)
		if err != nil {
			_ = listener.Close()
			return err
		}
		pool := x509.NewCertPool()
		ca, err := os.ReadFile(tlsConfig.ClientCA)
		if err != nil || !pool.AppendCertsFromPEM(ca) {
			_ = listener.Close()
			return errors.New("metrics client CA is invalid")
		}
		server.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}
		listener = tls.NewListener(listener, server.TLSConfig)
	}
	go shutdownOnCancel(ctx, server)
	err = server.Serve(listener)
	_ = listener.Close()
	return err
}

func networkAllowed(ip net.IP, networks []*net.IPNet) bool {
	for _, network := range networks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func shutdownOnCancel(ctx context.Context, server *http.Server) {
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
}

func filepathDir(path string) string {
	index := strings.LastIndexByte(path, '/')
	if index <= 0 {
		return "."
	}
	return path[:index]
}

func MetricsAddressRequiresTLS(address string) bool {
	host, _, err := net.SplitHostPort(address)
	return err != nil || host == "" || !isLoopbackHost(host)
}

func statusCode(value int) string { return strconv.Itoa(value) }
