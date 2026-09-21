package streamtransport

import (
	"context"
	"io"
	"net"
	"testing"
)

func TestFakeNetworkRequiresAllowlistAndCarriesBytes(t *testing.T) {
	network := NewFakeNetwork()
	publisher, err := network.StartPublisher(context.Background(), PublisherConfig{HandleConn: func(conn net.Conn) {
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { publisher.Close() })
	viewer, err := network.NewViewer()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { viewer.Close() })
	if err := viewer.SetConnectionBlob(publisher.ConnectionBlob()); err != nil {
		t.Fatal(err)
	}
	if _, err := viewer.Dial(context.Background()); err == nil {
		t.Fatal("unapproved viewer connected")
	}
	if err := publisher.AllowClient(viewer.PublicKey()); err != nil {
		t.Fatal(err)
	}
	conn, err := viewer.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 5)
	if _, err := io.ReadFull(conn, buffer); err != nil {
		t.Fatal(err)
	}
	if string(buffer) != "hello" {
		t.Fatalf("unexpected echo: %q", buffer)
	}
}
