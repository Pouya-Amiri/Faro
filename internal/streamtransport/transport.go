// Package streamtransport defines Faro's media data-plane transport boundary.
// Implementations carry authenticated Faro HTTP requests over byte streams;
// transport-specific types must not escape this package boundary.
package streamtransport

import (
	"context"
	"net"
)

// MediaPort is a virtual Tailcat port, not an operating-system listener.
const MediaPort uint16 = 47821

type PublisherConfig struct {
	DERPMapURL string
	HandleConn func(net.Conn)
}

type Publisher interface {
	ConnectionBlob() string
	AllowClient(publicKey string) error
	Close() error
}

type Viewer interface {
	PublicKey() string
	SetConnectionBlob(string) error
	WaitForDirect(context.Context) error
	Dial(context.Context) (net.Conn, error)
	Close() error
}

type Factory interface {
	StartPublisher(context.Context, PublisherConfig) (Publisher, error)
	NewViewer() (Viewer, error)
}
