package tailcat

import (
	"context"
	"net"
	"testing"

	"github.com/Pouya-Amiri/Faro/internal/streamtransport"
)

func TestViewerIdentityPrecedesConnectionBlob(t *testing.T) {
	viewer, err := (Factory{}).NewViewer()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { viewer.Close() })
	if viewer.PublicKey() == "" {
		t.Fatal("Tailcat viewer returned an empty public key")
	}
	if _, err := viewer.Dial(context.Background()); err == nil {
		t.Fatal("viewer dialed without a connection blob")
	}
	if err := viewer.SetConnectionBlob("not-a-tailcat-blob"); err == nil {
		t.Fatal("viewer accepted an invalid Tailcat connection blob")
	}
}

func TestPublisherRejectsInsecureCustomBootstrapMap(t *testing.T) {
	_, err := (Factory{}).StartPublisher(context.Background(), streamtransport.PublisherConfig{
		DERPMapURL: "http://relay.example/derpmap.json", HandleConn: func(net.Conn) {},
	})
	if err == nil {
		t.Fatal("publisher accepted an insecure custom bootstrap map")
	}
}
