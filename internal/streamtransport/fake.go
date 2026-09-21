package streamtransport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
)

// FakeNetwork is a deterministic in-memory transport for app and transfer
// integration tests. It enforces the same explicit client allowlist expected
// from the Tailcat adapter.
type FakeNetwork struct {
	mu         sync.Mutex
	next       uint64
	publishers map[string]*fakePublisher
}

func NewFakeNetwork() *FakeNetwork {
	return &FakeNetwork{publishers: make(map[string]*fakePublisher)}
}

func (n *FakeNetwork) StartPublisher(ctx context.Context, cfg PublisherConfig) (Publisher, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg.HandleConn == nil {
		return nil, errors.New("stream transport publisher requires a connection handler")
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.next++
	blob := fmt.Sprintf("fake-publisher-%d", n.next)
	p := &fakePublisher{network: n, blob: blob, handle: cfg.HandleConn, allowed: make(map[string]struct{})}
	n.publishers[blob] = p
	return p, nil
}

func (n *FakeNetwork) NewViewer() (Viewer, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.next++
	return &fakeViewer{network: n, publicKey: fmt.Sprintf("fake-client-%d", n.next)}, nil
}

type fakePublisher struct {
	network *FakeNetwork
	blob    string
	handle  func(net.Conn)

	mu      sync.Mutex
	allowed map[string]struct{}
	closed  bool
}

func (p *fakePublisher) ConnectionBlob() string { return p.blob }

func (p *fakePublisher) AllowClient(publicKey string) error {
	if publicKey == "" {
		return errors.New("stream transport client public key is required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return net.ErrClosed
	}
	p.allowed[publicKey] = struct{}{}
	return nil
}

func (p *fakePublisher) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()
	p.network.mu.Lock()
	delete(p.network.publishers, p.blob)
	p.network.mu.Unlock()
	return nil
}

type fakeViewer struct {
	network   *FakeNetwork
	blob      string
	publicKey string

	mu     sync.Mutex
	closed bool
}

func (v *fakeViewer) PublicKey() string { return v.publicKey }

func (v *fakeViewer) SetConnectionBlob(connectionBlob string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return net.ErrClosed
	}
	if v.blob != "" {
		return errors.New("stream transport connection blob is already set")
	}
	v.network.mu.Lock()
	publisher := v.network.publishers[connectionBlob]
	v.network.mu.Unlock()
	if publisher == nil {
		return errors.New("stream transport connection blob is invalid")
	}
	v.blob = connectionBlob
	return nil
}

func (v *fakeViewer) WaitForDirect(ctx context.Context) error { return ctx.Err() }

func (v *fakeViewer) Dial(ctx context.Context) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	v.mu.Lock()
	closed := v.closed
	v.mu.Unlock()
	if closed {
		return nil, net.ErrClosed
	}
	if v.blob == "" {
		return nil, errors.New("stream transport connection blob is not set")
	}
	v.network.mu.Lock()
	p := v.network.publishers[v.blob]
	v.network.mu.Unlock()
	if p == nil {
		return nil, net.ErrClosed
	}
	p.mu.Lock()
	_, allowed := p.allowed[v.publicKey]
	closed = p.closed
	handle := p.handle
	p.mu.Unlock()
	if closed {
		return nil, net.ErrClosed
	}
	if !allowed {
		return nil, errors.New("stream transport client is not allowed")
	}
	client, server := net.Pipe()
	go handle(server)
	return &contextConn{Conn: client, ctx: ctx}, nil
}

func (v *fakeViewer) Close() error {
	v.mu.Lock()
	v.closed = true
	v.mu.Unlock()
	return nil
}

type contextConn struct {
	net.Conn
	ctx context.Context
}

func (c *contextConn) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.Conn.Read(p)
}

func (c *contextConn) Write(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.Conn.Write(p)
}
