package main

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/pion/turn/v4"
	mmpb "npln.nintendo.net/npln-practice/proto/matchmaking/v1"
)

func TestTURNAllocationAndRelayRoundTrip(t *testing.T) {
	server, err := startTURN(nil, "127.0.0.1:0", "127.0.0.1", defaultTURNUsername, defaultTURNPassword, defaultTURNRealm)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	clientConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	client, err := turn.NewClient(&turn.ClientConfig{
		STUNServerAddr: server.address.String(),
		TURNServerAddr: server.address.String(),
		Conn:           clientConn,
		Username:       defaultTURNUsername,
		Password:       defaultTURNPassword,
		Realm:          defaultTURNRealm,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err = client.Listen(); err != nil {
		t.Fatal(err)
	}
	relay, err := client.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()

	peer, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	go func() {
		buffer := make([]byte, 32)
		n, from, readErr := peer.ReadFrom(buffer)
		if readErr == nil {
			_, _ = peer.WriteTo(append([]byte("reply:"), buffer[:n]...), from)
		}
	}()

	if _, err = relay.WriteTo([]byte("wonder"), peer.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	if err = relay.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 32)
	n, _, err := relay.ReadFrom(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buffer[:n]); got != "reply:wonder" {
		t.Fatalf("relayed response = %q", got)
	}
}

func TestAllocateIceServerSetAdvertisesTURN(t *testing.T) {
	response, err := newGameSessionServer(newSessionRegistry()).AllocateIceServerSet(
		context.Background(),
		&mmpb.AllocateIceServerSetRequest{Tenant: nplnTenant},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.GetTurnServers()) != 1 {
		t.Fatalf("TURN servers = %d", len(response.GetTurnServers()))
	}
	server := response.GetTurnServers()[0]
	if server.GetHost() != defaultTURNHost || server.GetPort() != 3479 || server.GetProtocol() != mmpb.TurnServer_UDP ||
		server.GetUsername() != defaultTURNUsername || server.GetPassword() != defaultTURNPassword {
		t.Fatalf("unexpected TURN advertisement: %+v", server)
	}
}
