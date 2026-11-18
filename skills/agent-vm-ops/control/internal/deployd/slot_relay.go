package deployd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/config"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/controllerrole"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/protocol"
)

const slotRelaySocket = "/run/agent-vm-slot-relay/relay.sock"
const slotRelayRequestLimit = 2 * protocol.DeploymentSlotBodyLimit
const slotRelayResponseLimit = 4 * 1024 * 1024
const recoveryMarkerRoute = "/internal/v1/recovery-marker"

var legacyMarkerID = regexp.MustCompile(`^point-[a-f0-9]{32}$`)

type slotRelayRequest struct {
	Slot, Method, Path string
	Body               json.RawMessage `json:",omitempty"`
	WatchLeaseOwner    bool            `json:",omitempty"`
}

type slotRelayResponse struct {
	Body   map[string]any
	Status int
}

func (request slotRelayRequest) valid() bool {
	if request.WatchLeaseOwner {
		return request.Slot == "" && request.Method == "" && request.Path == "" && len(request.Body) == 0
	}
	if validSlot(request.Slot) != nil || !protocol.DeploymentSlotRoute(request.Method, request.Path) || len(request.Body) > protocol.DeploymentSlotBodyLimit {
		return false
	}
	if request.Path == "/internal/v1/control" {
		control, err := protocol.Decode(request.Body)
		return err == nil && protocol.DeploymentSlotAction(control)
	}
	if request.Path == recoveryMarkerRoute {
		return request.recoveryMarkerID() != ""
	}
	return true
}

func (request slotRelayRequest) recoveryMarkerID() string {
	var marker struct {
		ID string `json:"id"`
	}
	if request.Method != http.MethodPost || readSlotRelayMessage(bytes.NewReader(request.Body), 256, &marker) != nil || !legacyMarkerID.MatchString(marker.ID) {
		return ""
	}
	return marker.ID
}

func RunSlotRelay(input *os.File) error {
	if !isServiceAccount(os.Geteuid(), slotUser) {
		return errors.New("slot relay must run under the server account")
	}
	conn, err := net.FileConn(input)
	if err != nil {
		return err
	}
	defer conn.Close()
	peer, err := peerUID(conn)
	if err != nil || !isDeployAccount(peer) {
		return errors.New("unauthorized slot relay peer")
	}
	if err := conn.SetDeadline(time.Now().Add(4 * time.Second)); err != nil {
		return err
	}
	reader := bufio.NewReader(io.LimitReader(conn, slotRelayRequestLimit+1))
	line, err := reader.ReadBytes('\n')
	var request slotRelayRequest
	if err != nil || readSlotRelayMessage(bytes.NewReader(line), slotRelayRequestLimit, &request) != nil || !request.valid() {
		return errors.New("invalid slot relay request")
	}
	if request.WatchLeaseOwner {
		return watchLegacyLeaseOwner(conn, reader)
	}
	if _, err := reader.ReadByte(); err != io.EOF {
		return errors.New("slot relay request contains trailing data")
	}
	if err := conn.SetDeadline(time.Now().Add(245 * time.Second)); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	runtime := NewHostRuntime(Defaults())
	if bytes.Equal(request.Body, []byte("null")) {
		request.Body = nil
	}
	result, status, err := callSlot(ctx, runtime.socket(request.Slot), request.Method, request.Path, request.Body, 240*time.Second, os.Getuid())
	if request.Path == recoveryMarkerRoute && status == http.StatusNotFound {
		if err := runtime.writeHistoricalRecoveryMarker(ctx, request.Slot, request.recoveryMarkerID()); err != nil {
			return err
		}
		result, status, err = map[string]any{"ok": true}, http.StatusOK, nil
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(conn).Encode(slotRelayResponse{Body: result, Status: status})
}

// Historical slots have no marker route. Reuse the server identity and only
// the fixed coverage write, after checking that this slot still owns traffic.
func (runtime *HostRuntime) writeHistoricalRecoveryMarker(ctx context.Context, slot, id string) error {
	if validSlot(slot) != nil || !legacyMarkerID.MatchString(id) {
		return errors.New("invalid historical recovery marker request")
	}
	selected, err := runtime.activeSlot()
	if err != nil || selected != slot {
		return errors.New("historical recovery marker requires the selected slot")
	}
	health, status, err := callSlot(ctx, runtime.socket(slot), http.MethodGet, "/internal/v1/health", nil, 3*time.Second, os.Getuid())
	if err != nil || status != http.StatusOK || !healthEnvelopeValid(health) || health["controller_owned"] != true ||
		health["traffic_accepting"] != true || health["ready"] != true {
		return errors.New("historical recovery marker requires a serving controller")
	}
	cfg, err := config.Load(filepath.Join("/etc/agent-vm-native/slots", slot, "config.json"))
	if err != nil || cfg.Slot != slot || config.ValidateInstallationAuthority(cfg.StateDB) != nil {
		return errors.New("invalid historical slot state authority")
	}
	dsn, _ := url.Parse(cfg.StateDB)
	password, _ := dsn.User.Password()
	ca, err := readRecoveryConfig(ServerAuthorityCA, false)
	if err != nil {
		return err
	}
	client, err := recoveryTLSClient(ca)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	return writeRecoveryMarkerSQL(ctx, client, "https://127.0.0.1:4001", dsn.User.Username(), password, id)
}

type legacyLeaseOwner struct {
	PID       int
	StartTime string
}

func watchLegacyLeaseOwner(conn net.Conn, reader *bufio.Reader) error {
	pid := os.Getpid()
	owner := legacyLeaseOwner{PID: pid, StartTime: controllerrole.ProcessStartToken(pid)}
	if owner.StartTime == "" {
		return errors.New("legacy lease witness identity unavailable")
	}
	if err := json.NewEncoder(conn).Encode(owner); err != nil {
		return err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return err
	}
	// Closing the authenticated caller's socket ends this process and causes
	// old slots to fence their lease using their original process check.
	if _, err := reader.ReadByte(); err != io.EOF {
		return errors.New("legacy lease witness connection interrupted")
	}
	return nil
}

func connectLegacyLeaseOwner(ctx context.Context) (net.Conn, legacyLeaseOwner, error) {
	var owner legacyLeaseOwner
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", slotRelaySocket)
	if err != nil {
		return nil, owner, err
	}
	owner, err = readLegacyLeaseOwner(ctx, conn)
	if err != nil {
		_ = conn.Close()
		return nil, owner, err
	}
	return conn, owner, nil
}

func readLegacyLeaseOwner(ctx context.Context, conn net.Conn) (legacyLeaseOwner, error) {
	var owner legacyLeaseOwner
	deadline := time.Now().Add(4 * time.Second)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return owner, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if err := json.NewEncoder(conn).Encode(slotRelayRequest{WatchLeaseOwner: true}); err != nil {
		return owner, err
	}
	reply, err := bufio.NewReader(io.LimitReader(conn, 512)).ReadBytes('\n')
	if err != nil || readSlotRelayMessage(bytes.NewReader(reply), 512, &owner) != nil || owner.PID <= 0 ||
		owner.StartTime == "" || owner.StartTime != controllerrole.ProcessStartToken(owner.PID) {
		return owner, errors.New("legacy lease witness identity could not be verified")
	}
	return owner, conn.SetDeadline(time.Time{})
}

func requestSlotRelay(ctx context.Context, request slotRelayRequest, timeout time.Duration) (map[string]any, int, error) {
	if request.WatchLeaseOwner || !request.valid() {
		return nil, 0, errors.New("invalid slot relay request")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", slotRelaySocket)
	if err != nil {
		return nil, 0, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return nil, 0, err
	}
	if err := conn.(*net.UnixConn).CloseWrite(); err != nil {
		return nil, 0, err
	}
	var response slotRelayResponse
	if err := readSlotRelayMessage(conn, slotRelayResponseLimit, &response); err != nil {
		return nil, 0, err
	}
	if response.Body == nil || response.Status < 200 || response.Status > 599 {
		return nil, 0, errors.New("invalid slot relay response")
	}
	return response.Body, response.Status, nil
}

func readSlotRelayMessage(conn io.Reader, limit int64, value any) error {
	data, err := io.ReadAll(io.LimitReader(conn, limit+1))
	if err != nil || int64(len(data)) > limit {
		return errors.New("slot relay message exceeds limit or was interrupted")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("slot relay message contains trailing data")
	}
	return nil
}
