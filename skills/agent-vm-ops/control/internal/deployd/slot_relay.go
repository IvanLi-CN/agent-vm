package deployd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/protocol"
)

const slotRelaySocket = "/run/agent-vm-slot-relay/relay.sock"
const slotRelayRequestLimit = 2 * protocol.DeploymentSlotBodyLimit
const slotRelayResponseLimit = 4 * 1024 * 1024

type slotRelayRequest struct {
	Slot, Method, Path string
	Body               json.RawMessage
}

type slotRelayResponse struct {
	Body   map[string]any
	Status int
}

func (request slotRelayRequest) valid() bool {
	if validSlot(request.Slot) != nil || !protocol.DeploymentSlotRoute(request.Method, request.Path) || len(request.Body) > protocol.DeploymentSlotBodyLimit {
		return false
	}
	if request.Path == "/internal/v1/control" {
		control, err := protocol.Decode(request.Body)
		return err == nil && protocol.DeploymentSlotAction(control)
	}
	return true
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
	var request slotRelayRequest
	if err := readSlotRelayMessage(conn, slotRelayRequestLimit, &request); err != nil || !request.valid() {
		return errors.New("invalid slot relay request")
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
	if err != nil {
		return err
	}
	return json.NewEncoder(conn).Encode(slotRelayResponse{Body: result, Status: status})
}

func requestSlotRelay(ctx context.Context, request slotRelayRequest, timeout time.Duration) (map[string]any, int, error) {
	if !request.valid() {
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
