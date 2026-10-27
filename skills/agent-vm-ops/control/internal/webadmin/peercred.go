package webadmin

import (
	"context"
	"fmt"
	"net"
)

type peerUIDContextKey struct{}

// WithPeerUID is used by boundary tests to supply the same authenticated peer
// fact that ServeWeb and ServeAuthority attach from the Unix socket.
func WithPeerUID(ctx context.Context, uid int) context.Context {
	return context.WithValue(ctx, peerUIDContextKey{}, uid)
}

func peerUIDFromContext(ctx context.Context) (int, bool) {
	value, ok := ctx.Value(peerUIDContextKey{}).(int)
	return value, ok
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
	uid, err := peerUID(conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &peerConn{Conn: conn, uid: uid}, nil
}

func connContext(ctx context.Context, conn net.Conn) context.Context {
	if peer, ok := conn.(*peerConn); ok {
		return context.WithValue(ctx, peerUIDContextKey{}, peer.uid)
	}
	return ctx
}

func requirePeerUID(ctx context.Context, expected int) error {
	uid, ok := peerUIDFromContext(ctx)
	if !ok {
		return fmt.Errorf("peer credentials are unavailable")
	}
	if uid != expected {
		return fmt.Errorf("peer UID %d is not authorized", uid)
	}
	return nil
}
