package mediastream

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Pouya-Amiri/Faro/internal/protocol"
	"github.com/Pouya-Amiri/Faro/internal/streamtransport"
)

type noDirectFactory struct{ viewer *noDirectViewer }

func (f noDirectFactory) StartPublisher(context.Context, streamtransport.PublisherConfig) (streamtransport.Publisher, error) {
	return nil, errors.New("unused")
}

func (f noDirectFactory) NewViewer() (streamtransport.Viewer, error) { return f.viewer, nil }

type noDirectViewer struct{ closed bool }

func (*noDirectViewer) PublicKey() string                   { return "ephemeral-key" }
func (*noDirectViewer) SetConnectionBlob(string) error      { return nil }
func (*noDirectViewer) WaitForDirect(context.Context) error { return errors.New("no direct route") }
func (*noDirectViewer) Dial(context.Context) (net.Conn, error) {
	return nil, errors.New("must not dial")
}
func (v *noDirectViewer) Close() error { v.closed = true; return nil }

func TestPreparedViewerRefusesRelayedMedia(t *testing.T) {
	transportViewer := &noDirectViewer{}
	viewer, err := PrepareViewer(noDirectFactory{viewer: transportViewer})
	if err != nil {
		t.Fatal(err)
	}
	_, err = viewer.StartGateway(context.Background(), protocol.MediaStreamGranted{
		ConnectionBlob: "opaque", TransferCapability: strings.Repeat("c", 32),
		ExpiresAtUnixMs: time.Now().Add(time.Minute).UnixMilli(),
	}, protocol.Media{Title: "Movie.mkv", SizeBytes: 1024, Fingerprint: "file-v1:" + strings.Repeat("a", 64)})
	if err == nil || !strings.Contains(err.Error(), "direct") {
		t.Fatalf("unexpected direct-route error: %v", err)
	}
	if !transportViewer.closed {
		t.Fatal("viewer was not closed after direct-route failure")
	}
}
