package tailcat

import (
	"context"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

func TestRoomDirectTransportIntegration(t *testing.T) {
	if os.Getenv("FARO_TAILCAT_INTEGRATION") != "1" {
		t.Skip("set FARO_TAILCAT_INTEGRATION=1 for live rendezvous")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			defer conn.Close()
			io.Copy(conn, conn)
		}
	}()
	address, err := StartRoom(ctx, listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := RoomDialer(address)(ctx, "tcp", "peer:8999")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err = conn.Write([]byte("faro-peer")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 9)
	if _, err = io.ReadFull(conn, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "faro-peer" {
		t.Fatalf("unexpected echo %q", got)
	}
}
