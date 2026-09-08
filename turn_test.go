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

func TestAllocateIceServerSetAdvertisesConfiguredSTUN(t *testing.T) {
	t.Setenv("NPLN_STUN_HOST", "100.100.100.100")
	t.Setenv("NPLN_STUN_PORT", "13478")
	response, err := newGameSessionServer(newSessionRegistry()).AllocateIceServerSet(
		context.Background(),
		&mmpb.AllocateIceServerSetRequest{Tenant: nplnTenant},
	)
	if err != nil {
		t.Fatal(err)
	}
	server := response.GetStunServer()
	if server.GetHost() != "100.100.100.100" || server.GetPort() != 13478 || server.GetProtocol() != mmpb.StunServer_UDP {
		t.Fatalf("unexpected STUN advertisement: %+v", server)
	}
}

func TestAllocateIceServerSetPreservesLocalSTUNDefaults(t *testing.T) {
	t.Setenv("NPLN_STUN_HOST", "")
	t.Setenv("NPLN_STUN_PORT", "")
	response, err := newGameSessionServer(newSessionRegistry()).AllocateIceServerSet(context.Background(), &mmpb.AllocateIceServerSetRequest{Tenant: nplnTenant})
	if err != nil {
		t.Fatal(err)
	}
	s := response.GetStunServer()
	if s.GetHost() != "127.0.0.1" || s.GetPort() != 3478 || s.GetProtocol() != mmpb.StunServer_UDP {
		t.Fatalf("local STUN defaults changed: %+v", s)
	}
}

func TestLatencyEndpointConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, host, port, gameHost, wantHost string
		wantPort                             int32
	}{
		{"local defaults", "", "", "", "127.0.0.1", 443},
		{"game host fallback", "", "", "wonder.example.test", "wonder.example.test", 443},
		{"explicit endpoint", "latency.example.test", "8443", "wonder.example.test", "latency.example.test", 8443},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NPLN_LATENCY_HOST", tc.host)
			t.Setenv("NPLN_LATENCY_PORT", tc.port)
			t.Setenv("NPLN_GAMESESSION_HOST", tc.gameHost)
			response, err := newGameSessionServer(newSessionRegistry()).ListLatencyMeasurementServers(context.Background(), &mmpb.ListLatencyMeasurementServersRequest{Parent: nplnTenant})
			if err != nil {
				t.Fatal(err)
			}
			servers := response.GetLatencyMeasurementServers()
			if len(servers) != 1 {
				t.Fatalf("latency servers = %d", len(servers))
			}
			s := servers[0]
			if s.GetHost() != tc.wantHost || s.GetPort() != tc.wantPort || s.GetProtocol() != mmpb.LatencyMeasurementServer_HTTP {
				t.Fatalf("unexpected latency endpoint: %+v", s)
			}
		})
	}
}
