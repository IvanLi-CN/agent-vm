//go:build linux

package deployd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/protocol"
)

const requestRelayLimit = 2 << 20
const requestRelayTimeout = 10 * time.Minute
const RequestRelaySocket = "/run/agent-vm-native/control.sock"

// RunRequestRelay is a socket-activated root adapter, never a business authority.
// Only a local root caller can supply a request. The child connects using the
// fixed active socket's owner, including root during historical account migration.
func RunRequestRelay(input *os.File) error {
	if os.Geteuid() != 0 {
		return errors.New("request relay requires a root socket activation")
	}
	conn, err := net.FileConn(input)
	if err != nil {
		return err
	}
	defer conn.Close()
	uid, err := peerUID(conn)
	if err != nil || uid != 0 {
		return errors.New("unauthorized request relay peer")
	}
	_ = conn.SetDeadline(time.Now().Add(requestRelayTimeout))
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	body, err := readRelayHTTPRequest(conn)
	if err != nil {
		return writeRelayFailure(conn, err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	active, err := EffectiveActivePath(Defaults().ActivePath)
	if err != nil {
		return writeRelayFailure(conn, err)
	}
	slot := filepath.Base(active)
	if validSlot(slot) != nil || active != filepath.Join(Defaults().ReleaseRoot, "slots", slot) {
		return writeRelayFailure(conn, errors.New("active request slot is unavailable"))
	}
	credential, err := requestSlotCredential(slot)
	if err != nil {
		return writeRelayFailure(conn, err)
	}
	// An executable descriptor avoids opening historical package directories to
	// the service account. No executable supplied by a slot is run as root.
	self, err := os.Open("/proc/self/exe")
	if err != nil {
		return writeRelayFailure(conn, err)
	}
	defer self.Close()
	ctx, cancel := context.WithTimeout(context.Background(), requestRelayTimeout)
	defer cancel()
	child := exec.CommandContext(ctx, "/proc/self/fd/3", "request-relay-child", slot)
	child.ExtraFiles = []*os.File{self}
	child.SysProcAttr = &syscall.SysProcAttr{Credential: credential}
	child.Env = []string{"PATH=/usr/bin:/bin"}
	child.Stdin, child.Stdout, child.Stderr = bytes.NewReader(body), conn, os.Stderr
	// Once the child starts, a transport failure is uncertain: do not replay it
	// or append a second response to a possibly partially written response.
	if err := child.Start(); err != nil {
		return writeRelayFailure(conn, err)
	}
	return child.Wait()
}

func requestSlotCredential(slot string) (*syscall.Credential, error) {
	if validSlot(slot) != nil {
		return nil, errors.New("invalid request relay slot")
	}
	info, err := os.Lstat(filepath.Join("/run/agent-vm-native", slot, "app.sock"))
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return nil, errors.New("active request socket is unavailable")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, errors.New("active request socket identity is unavailable")
	}
	if st.Uid == 0 {
		return &syscall.Credential{Uid: 0, Gid: 0, Groups: []uint32{}}, nil
	}
	account, err := user.Lookup(slotUser)
	if err != nil || account.Uid != strconv.FormatUint(uint64(st.Uid), 10) {
		return nil, errors.New("active request socket has an unsupported owner")
	}
	primary, err := strconv.ParseUint(account.Gid, 10, 32)
	if err != nil {
		return nil, err
	}
	web, err := user.LookupGroup("agent-vm-web")
	if err != nil {
		return nil, err
	}
	group, err := strconv.ParseUint(web.Gid, 10, 32)
	if err != nil {
		return nil, err
	}
	return &syscall.Credential{Uid: st.Uid, Gid: uint32(group), Groups: []uint32{uint32(primary)}}, nil
}

func readRelayHTTPRequest(input io.Reader) ([]byte, error) {
	request, err := http.ReadRequest(bufio.NewReader(io.LimitReader(input, requestRelayLimit+8193)))
	if err != nil {
		return nil, errors.New("invalid request relay HTTP message")
	}
	defer request.Body.Close()
	if request.Method != http.MethodPost || request.RequestURI != "/internal/v1/control" {
		return nil, errors.New("request relay accepts only the fixed control route")
	}
	headerBytes := len(request.RequestURI) + len(request.Host)
	for key, values := range request.Header {
		for _, value := range values {
			headerBytes += len(key) + len(value) + 4
		}
	}
	if headerBytes > 8192 {
		return nil, errors.New("request relay headers exceed the limit")
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, requestRelayLimit+1))
	if err != nil || len(body) > requestRelayLimit {
		return nil, errors.New("request relay body exceeds the limit or is truncated")
	}
	if _, err := protocol.Decode(body); err != nil {
		return nil, errors.New("invalid request relay protocol body")
	}
	return body, nil
}

// RunRequestRelayChild is unprivileged once the active slot has been migrated.
// It accepts only a fixed slot and protocol body, not a URL, command or path.
func RunRequestRelayChild(slot string, input io.Reader, output io.Writer) error {
	credential, err := requestSlotCredential(slot)
	if err != nil || uint32(os.Geteuid()) != credential.Uid {
		return errors.New("request relay identity no longer matches the slot")
	}
	body, err := io.ReadAll(io.LimitReader(input, requestRelayLimit+1))
	if err != nil || len(body) > requestRelayLimit {
		return writeRelayFailure(output, errors.New("request relay body exceeds the limit"))
	}
	if _, err := protocol.Decode(body); err != nil {
		return writeRelayFailure(output, errors.New("invalid request relay protocol body"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestRelayTimeout)
	defer cancel()
	return forwardRequestRelay(ctx, filepath.Join("/run/agent-vm-native", slot, "app.sock"), body, output)
}

func forwardRequestRelay(ctx context.Context, socket string, body []byte, output io.Writer) error {
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dial net.Dialer
		return dial.DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/internal/v1/control", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return writeRelayFailure(output, errors.New("request relay transport failed; outcome may be uncertain"))
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, requestRelayLimit+1))
	if err != nil || len(payload) > requestRelayLimit {
		return writeRelayFailure(output, errors.New("request relay response is incomplete or exceeds the limit"))
	}
	response.Body = io.NopCloser(bytes.NewReader(payload))
	response.ContentLength, response.TransferEncoding, response.Close = int64(len(payload)), nil, true
	return response.Write(output)
}

func writeRelayFailure(output io.Writer, cause error) error {
	body, _ := json.Marshal(map[string]any{"ok": false, "protocol": protocol.Version, "error": protocol.NewError("control_transport_failed", cause.Error(), nil)})
	response := &http.Response{StatusCode: http.StatusServiceUnavailable, ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{"Content-Type": {"application/json"}, "X-Agent-Vm-Exit-Code": {"3"}},
		Body:   io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Close: true}
	if err := response.Write(output); err != nil {
		return fmt.Errorf("request relay response: %w", err)
	}
	return nil
}
