package tailcat

import (
	"context"
	"io"
	"net"
	"sync"
	"time"

	upstream "github.com/tailscale/tailcat"
	"tailscale.com/wgengine/filter"
)

const roomPort = 8999

// StartRoom forwards opaque TLS connections to the loopback Faro server.
// The private invite and pinned TLS authenticate room clients before hello.
func StartRoom(ctx context.Context, address string) (string, error) {
	server := &upstream.Server{
		ServedTCPPorts: []filter.PortRange{{First: roomPort, Last: roomPort}},
		Logf:           discardLog,
		OnTCP: func(port uint16) func(net.Conn) {
			if port != roomPort {
				return nil
			}
			return func(peer net.Conn) {
				defer peer.Close()
				local, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
				if err != nil {
					return
				}
				defer local.Close()
				stop := context.AfterFunc(ctx, func() { peer.Close(); local.Close() })
				defer stop()
				done := make(chan struct{})
				go func() { io.Copy(local, peer); local.Close(); close(done) }()
				io.Copy(peer, local)
				peer.Close()
				<-done
			}
		},
	}
	started := make(chan error, 1)
	go func() { started <- server.Start() }()
	startCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	select {
	case err := <-started:
		if err != nil {
			return "", err
		}
	case <-startCtx.Done():
		go func() {
			if err := <-started; err == nil {
				server.Close()
			}
		}()
		return "", startCtx.Err()
	}
	go func() { <-ctx.Done(); server.Close() }()
	return string(server.TailcatAddr()), nil
}

// RoomDialer owns one ephemeral Tailcat client per Faro connection.
func RoomDialer(address string) func(context.Context, string, string) (net.Conn, error) {
	if address == "" {
		return nil
	}
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		if _, err := upstream.ParseAddr(upstream.Addr(address)); err != nil {
			return nil, err
		}
		client := &upstream.Client{Server: upstream.Addr(address), Logf: discardLog}
		route := &viewer{client: client, set: true}
		if err := route.WaitForDirect(ctx); err != nil {
			client.Close()
			return nil, err
		}
		conn, err := client.DialTCPPort(ctx, roomPort)
		if err != nil {
			client.Close()
			return nil, err
		}
		return &roomConn{Conn: conn, client: client}, nil
	}
}

type roomConn struct {
	net.Conn
	client *upstream.Client
	once   sync.Once
}

func (c *roomConn) Close() error {
	var err error
	c.once.Do(func() { err = c.Conn.Close(); c.client.Close() })
	return err
}
