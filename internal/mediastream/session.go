package mediastream

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"time"

	"github.com/Pouya-Amiri/Faro/internal/protocol"
	"github.com/Pouya-Amiri/Faro/internal/streamtransport"
)

type HostedPublisherConfig struct {
	Source     SourceConfig
	DERPMapURL string
}

// HostedPublisher owns the validated file handle and the ephemeral transport
// server for one offer. It never exposes the local path after construction.
type HostedPublisher struct {
	source   *Source
	endpoint streamtransport.Publisher
	once     sync.Once
}

func StartHostedPublisher(ctx context.Context, factory streamtransport.Factory, cfg HostedPublisherConfig) (*HostedPublisher, error) {
	if factory == nil {
		return nil, errors.New("stream transport factory is required")
	}
	source, err := NewSource(cfg.Source)
	if err != nil {
		return nil, err
	}
	endpoint, err := factory.StartPublisher(ctx, streamtransport.PublisherConfig{
		DERPMapURL: cfg.DERPMapURL, HandleConn: source.HandleConn,
	})
	if err != nil {
		source.Close()
		return nil, err
	}
	return &HostedPublisher{source: source, endpoint: endpoint}, nil
}

func (p *HostedPublisher) ConnectionBlob() string { return p.endpoint.ConnectionBlob() }

func (p *HostedPublisher) Grant(clientPublicKey string, expiresAt time.Time) (string, error) {
	if !expiresAt.After(time.Now()) {
		return "", errors.New("stream grant expiry must be in the future")
	}
	if err := p.endpoint.AllowClient(clientPublicKey); err != nil {
		return "", err
	}
	capability, err := randomCapability()
	if err != nil {
		return "", err
	}
	if err := p.source.Authorize(capability, expiresAt); err != nil {
		return "", err
	}
	return capability, nil
}

func (p *HostedPublisher) Revoke(capability string) { p.source.Revoke(capability) }

func (p *HostedPublisher) Close() error {
	var result error
	p.once.Do(func() { result = errors.Join(p.endpoint.Close(), p.source.Close()) })
	return result
}

type PreparedViewer struct {
	viewer streamtransport.Viewer
	mu     sync.Mutex
	used   bool
}

func PrepareViewer(factory streamtransport.Factory) (*PreparedViewer, error) {
	if factory == nil {
		return nil, errors.New("stream transport factory is required")
	}
	viewer, err := factory.NewViewer()
	if err != nil {
		return nil, err
	}
	return &PreparedViewer{viewer: viewer}, nil
}

func (v *PreparedViewer) PublicKey() string { return v.viewer.PublicKey() }

func (v *PreparedViewer) StartGateway(ctx context.Context, grant protocol.MediaStreamGranted, media protocol.Media) (*Gateway, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.used {
		return nil, errors.New("stream viewer grant was already consumed")
	}
	if !time.UnixMilli(grant.ExpiresAtUnixMs).After(time.Now()) {
		return nil, errors.New("stream grant has expired")
	}
	v.used = true
	if err := v.viewer.SetConnectionBlob(grant.ConnectionBlob); err != nil {
		return nil, err
	}
	if err := v.viewer.WaitForDirect(ctx); err != nil {
		v.viewer.Close()
		return nil, err
	}
	gateway, err := NewGateway(GatewayConfig{Viewer: v.viewer, Capability: grant.TransferCapability, Media: media})
	if err != nil {
		v.viewer.Close()
		return nil, err
	}
	return gateway, nil
}

func (v *PreparedViewer) Close() error { return v.viewer.Close() }

func randomCapability() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}
