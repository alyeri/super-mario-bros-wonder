package main

// Nextendo NPLN Server — Super Mario Bros. Wonder (Title ID: 010015100B514000, Tenant: t-ba973ec6-lp1)
//
// Standalone reference backend for the Wonder NPLN service flow used by Nextendo.

import (
	"context"
	"crypto/tls"
	"log"
	"net"
	"os"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"

	authpb "npln.nintendo.net/npln-practice/proto/auth/v1"
	friendspb "npln.nintendo.net/npln-practice/proto/friends/v1"
	gspb "npln.nintendo.net/npln-practice/proto/gamesync/v1"
	hydropb "npln.nintendo.net/npln-practice/proto/hydro/v1"
	mmpb "npln.nintendo.net/npln-practice/proto/matchmaking/v1"
	msgpb "npln.nintendo.net/npln-practice/proto/messaging/v1"
	ugcpb "npln.nintendo.net/npln-practice/proto/ugcstore/v1"
)

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func envInt(k string, d int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return d
}

func envDuration(k string, d time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if parsed, err := time.ParseDuration(v); err == nil && parsed > 0 {
			return parsed
		}
	}
	return d
}

func logMetadata(ctx context.Context, method string) {
	md, _ := metadata.FromIncomingContext(ctx)
	get := func(k string) string {
		if v := md.Get(k); len(v) > 0 {
			return v[0]
		}
		return ""
	}
	auth := get("authorization")
	if auth != "" {
		auth = redactMetadataValue(auth)
	}
	log.Printf("[NPLN RPC] %s | tenant=%q | uid=%q | auth=%q",
		method, get("npln-tenant-id"), get("uid"), auth)
}

func traceInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	logMetadata(ctx, info.FullMethod)
	wonderProbe.captureMessage(ctx, "in", info.FullMethod, req)
	resp, err := handler(ctx, req)
	if err == nil && resp != nil {
		wonderProbe.captureMessage(ctx, "out", info.FullMethod, resp)
	}
	logRPCResult(info.FullMethod, err)
	return resp, err
}

func buildServer(creds credentials.TransportCredentials) *grpc.Server {
	maxMessageBytes := envInt("NPLN_MAX_MESSAGE_BYTES", 32<<20)
	opts := []grpc.ServerOption{
		grpc.UnaryInterceptor(traceInterceptor),
		grpc.StreamInterceptor(traceStreamInterceptor),
		grpc.ForceServerCodec(newHybridCodec()),
		grpc.UnknownServiceHandler(replayHandler),
		grpc.StatsHandler(wonderProbe),
		grpc.MaxRecvMsgSize(maxMessageBytes),
		grpc.MaxSendMsgSize(maxMessageBytes),
		// Permit early client pings during protocol discovery. This is a
		// compatibility setting, not evidence for any specific GOAWAY cause.
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             5 * time.Second,
			PermitWithoutStream: true,
		}),
	}
	if creds != nil {
		opts = append(opts, grpc.Creds(creds))
	}

	s := grpc.NewServer(opts...)
	sessions := newSessionRegistry()

	// Register Core NPLN Services
	authpb.RegisterAuthServer(s, &authServer{})
	friendspb.RegisterFriendsServer(s, &friendsServer{})
	friendspb.RegisterPresenceServiceServer(s, &presenceServer{})
	gspb.RegisterGamesyncServer(s, newGamesyncServer(sessions))
	hydropb.RegisterDatastoreServer(s, newDatastoreServer())
	mmpb.RegisterMatchmakerServer(s, newMatchmaker(sessions))
	mmpb.RegisterGameSessionServiceServer(s, newGameSessionServer(sessions))
	msgpb.RegisterMessagingServer(s, &messagingServer{})
	ugcpb.RegisterUgcstoreServer(s, &ugcstoreServer{})

	wonderProbe.record(probeEvent{
		Kind: "server_manifest",
		Fields: map[string]interface{}{
			"services":          sortedServiceManifest(s),
			"max_message_bytes": maxMessageBytes,
			"unknown_rpc_mode":  envOr("NPLN_UNKNOWN_RPC_MODE", "unimplemented"),
		},
	})

	return s
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	log.Printf("================================================================")
	log.Printf(" Nextendo NPLN Server — Super Mario Bros. Wonder [010015100B514000]")
	log.Printf(" Tenant ID: %s | Server ID: ba973ec6", nplnTenantID)
	log.Printf("================================================================")

	addr := envOr("NPLN_LISTEN", "0.0.0.0:443")
	certFile := envOr("CERT_FILE", "cert.pem")
	keyFile := envOr("KEY_FILE", "key.pem")

	ensureTlsCertificate(certFile, keyFile)
	log.Printf("[NPLN PROBE] Structured evidence directory: %s", wonderProbe.dir)
	udpProbe, err := startUDPObservers(wonderProbe, configuredUDPObserverAddresses())
	if err != nil {
		log.Fatalf("Failed to start passive UDP observers: %v", err)
	}
	defer udpProbe.Close()
	stun, err := startSTUN(wonderProbe, envOr("NPLN_STUN_LISTEN", "127.0.0.1:3478"))
	if err != nil {
		log.Fatalf("Failed to start advertised STUN endpoint: %v", err)
	}
	defer stun.Close()
	turnRelay, err := startTURN(
		wonderProbe,
		envOr("NPLN_TURN_LISTEN", defaultTURNListen),
		envOr("NPLN_TURN_RELAY_IP", defaultTURNHost),
		envOr("NPLN_TURN_USERNAME", defaultTURNUsername),
		envOr("NPLN_TURN_PASSWORD", defaultTURNPassword),
		envOr("NPLN_TURN_REALM", defaultTURNRealm),
	)
	if err != nil {
		log.Fatalf("Failed to start advertised TURN endpoint: %v", err)
	}
	defer turnRelay.Close()

	nncsConfig, nncsEnabled, err := configuredNNCS()
	if err != nil {
		log.Fatalf("Invalid NNCS configuration: %v", err)
	}
	var nncs *nncsServer
	if nncsEnabled {
		nncs, err = startNNCSServer(wonderProbe, nncsConfig)
		if err != nil {
			log.Fatalf("Failed to start NNCS: %v", err)
		}
		defer nncs.Close()
	} else {
		wonderProbe.record(probeEvent{Kind: "nncs_disabled"})
		log.Printf("[NNCS] Disabled by NPLN_NNCS_ENABLED")
	}

	if os.Getenv("NPLN_STANDALONE_GRPC") != "" {
		tlsCert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			log.Fatalf("Failed to load TLS certificates: %v", err)
		}
		creds := credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{tlsCert},
			NextProtos:   []string{"h2", "grpc-exp"},
			MinVersion:   tls.VersionTLS12,
		})
		lis, err := net.Listen("tcp", addr)
		if err != nil {
			log.Fatalf("Failed to listen on %s: %v", addr, err)
		}
		log.Printf("Standalone gRPC Server listening on %s (TLS)", addr)
		log.Fatal(buildServer(creds).Serve(lis))
	}

	// Default combined mode: ALPN Demuxer on port 443
	startLocalCombined(addr, certFile, keyFile)
}
