// Package tailcat contains the only Faro code allowed to depend on Tailcat's
// unstable Go API. Keep conversions to opaque strings at this boundary.
package tailcat

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Pouya-Amiri/Faro/internal/streamtransport"
	upstream "github.com/tailscale/tailcat"
	"tailscale.com/types/key"
	"tailscale.com/wgengine/filter"
)

type Factory struct{}

func (Factory) StartPublisher(ctx context.Context, cfg streamtransport.PublisherConfig) (streamtransport.Publisher, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg.HandleConn == nil {
		return nil, errors.New("tailcat publisher requires a connection handler")
	}
	derpMapURL := strings.TrimSpace(cfg.DERPMapURL)
	if derpMapURL != "" {
		parsed, err := url.Parse(derpMapURL)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
			return nil, errors.New("Tailcat bootstrap map must be an absolute HTTPS URL")
		}
		derpMapURL = parsed.String()
	}
	// Tailcat treats an empty allowlist as allow-all. Seed it with a key that
	// is never returned so the server is deny-by-default before the first grant.
	server := &upstream.Server{
		DERPMapURL:     derpMapURL,
		AllowedClients: []key.NodePublic{key.NewNode().Public()},
		ServedTCPPorts: []filter.PortRange{{First: streamtransport.MediaPort, Last: streamtransport.MediaPort}},
		Logf:           discardLog,
		OnTCP: func(port uint16) func(net.Conn) {
			if port != streamtransport.MediaPort {
				return nil
			}
			return cfg.HandleConn
		},
	}
	if err := server.Start(); err != nil {
		return nil, err
	}
	return &publisher{server: server, blob: string(server.TailcatAddr())}, nil
}

func (Factory) NewViewer() (streamtransport.Viewer, error) {
	return &viewer{client: &upstream.Client{Logf: discardLog}}, nil
}

type publisher struct {
	server *upstream.Server
	blob   string
}

func (p *publisher) ConnectionBlob() string { return p.blob }

func (p *publisher) AllowClient(publicKey string) error {
	var parsed key.NodePublic
	if err := parsed.UnmarshalText([]byte(strings.TrimSpace(publicKey))); err != nil {
		return err
	}
	p.server.AddAllowedClient(parsed)
	return nil
}

func (p *publisher) Close() error { return p.server.Close() }

type viewer struct {
	client        *upstream.Client
	mu            sync.Mutex
	set           bool
	directChecked time.Time
	directMu      sync.Mutex
}

func (v *viewer) PublicKey() string { return v.client.PublicKey().String() }

func (v *viewer) SetConnectionBlob(connectionBlob string) error {
	connectionBlob = strings.TrimSpace(connectionBlob)
	if connectionBlob == "" {
		return errors.New("tailcat connection blob is required")
	}
	if _, err := upstream.ParseAddr(upstream.Addr(connectionBlob)); err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.set {
		return errors.New("tailcat connection blob is already set")
	}
	v.client.Server = upstream.Addr(connectionBlob)
	v.set = true
	return nil
}

// WaitForDirect uses DERP only for Tailcat's encrypted rendezvous handshake,
// then refuses the media connection unless NAT traversal established a direct
// peer-to-peer UDP path. It rechecks periodically so a degraded route does not
// silently turn into relayed media.
func (v *viewer) WaitForDirect(ctx context.Context) error {
	v.directMu.Lock()
	defer v.directMu.Unlock()
	v.mu.Lock()
	set, fresh := v.set, time.Since(v.directChecked) < 5*time.Second
	v.mu.Unlock()
	if !set {
		return errors.New("tailcat connection blob is not set")
	}
	if fresh {
		return nil
	}
	directCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	for {
		result, err := v.client.DiscoPing(directCtx)
		if err != nil {
			if directCtx.Err() != nil {
				return directPathError()
			}
			return err
		}
		if result.Endpoint != "" {
			v.mu.Lock()
			v.directChecked = time.Now()
			v.mu.Unlock()
			return nil
		}
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-timer.C:
		case <-directCtx.Done():
			timer.Stop()
			return directPathError()
		}
	}
}

func (v *viewer) Dial(ctx context.Context) (net.Conn, error) {
	v.mu.Lock()
	set := v.set
	v.mu.Unlock()
	if !set {
		return nil, errors.New("tailcat connection blob is not set")
	}
	return v.client.DialTCPPort(ctx, streamtransport.MediaPort)
}

func (v *viewer) Close() error { return v.client.Close() }

func discardLog(string, ...any) {}

func directPathError() error {
	return errors.New("direct peer-to-peer path unavailable (check UDP/firewall access; use Advanced hosting for rooms or a local copy for shared media)")
}

var _ streamtransport.Factory = Factory{}
