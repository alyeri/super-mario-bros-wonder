package main

// local — Local TLS Demultiplexer on port 443.
// Demuxes incoming TLS connections by ALPN into gRPC (h2) and REST/HTTP (http/1.1).

import (
	"crypto/tls"
	"log"
	"net"
	"net/http"
	"time"
)

type chanListener struct {
	ch   chan net.Conn
	addr net.Addr
	done chan struct{}
}

func newChanListener(addr net.Addr) *chanListener {
	return &chanListener{ch: make(chan net.Conn, 32), addr: addr, done: make(chan struct{})}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error   { close(l.done); return nil }
func (l *chanListener) Addr() net.Addr { return l.addr }

func (l *chanListener) push(c net.Conn) {
	select {
	case l.ch <- c:
	case <-l.done:
		_ = c.Close()
	}
}

func buildRestMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[NPLN REST] %s %s %s", r.Method, r.URL.Path, r.RemoteAddr)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	return mux
}

func startLocalCombined(addr, certFile, keyFile string) {
	tlsCert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		log.Fatalf("[NPLN LOCAL] load cert error: %v", err)
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		NextProtos:   []string{"h2", "http/1.1", "grpc-exp"},
		MinVersion:   tls.VersionTLS12,
		GetConfigForClient: func(hi *tls.ClientHelloInfo) (*tls.Config, error) {
			connID := wonderProbe.connectionID(hi.Conn.RemoteAddr())
			log.Printf("[NPLN TLS] ClientHello SNI=%q ClientAddr=%v ALPN=%v",
				hi.ServerName, hi.Conn.RemoteAddr(), hi.SupportedProtos)
			wonderProbe.record(probeEvent{
				Kind:         "tls_client_hello",
				ConnectionID: connID,
				Fields: map[string]interface{}{
					"sni":                hi.ServerName,
					"remote":             addressString(hi.Conn.RemoteAddr()),
					"local":              addressString(hi.Conn.LocalAddr()),
					"supported_alpn":     hi.SupportedProtos,
					"supported_versions": hi.SupportedVersions,
					"cipher_suites":      hi.CipherSuites,
					"signature_schemes":  hi.SignatureSchemes,
					"supported_curves":   hi.SupportedCurves,
				},
			})
			return nil, nil
		},
	}
	wonderProbe.record(probeEvent{
		Kind: "tls_server_config",
		Fields: map[string]interface{}{
			"listen":                 addr,
			"server_alpn_preference": tlsCfg.NextProtos,
			"minimum_version":        tls.VersionName(tlsCfg.MinVersion),
			"handshake_timeout":      envDuration("NPLN_TLS_HANDSHAKE_TIMEOUT", 10*time.Second).String(),
			"certificate":            tlsCertificateFields(tlsCert),
		},
	})

	ln, err := tls.Listen("tcp", addr, tlsCfg)
	if err != nil {
		log.Fatalf("[NPLN LOCAL] listen on %s failed: %v", addr, err)
	}

	grpcSrv := buildServer(nil) // Decrypted h2 served directly
	restSrv := &http.Server{Handler: buildRestMux()}
	grpcLn := newChanListener(ln.Addr())
	restLn := newChanListener(ln.Addr())

	go func() {
		if err := grpcSrv.Serve(grpcLn); err != nil {
			log.Printf("[NPLN LOCAL] gRPC serve exited: %v", err)
		}
	}()
	go func() {
		if err := restSrv.Serve(restLn); err != nil {
			log.Printf("[NPLN LOCAL] REST serve exited: %v", err)
		}
	}()

	log.Printf("[NPLN LOCAL] Nextendo NPLN server listening on %s (TLS ALPN h2+http/1.1) for Wonder", addr)
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Printf("[NPLN LOCAL] accept error: %v", err)
			continue
		}
		go func(c net.Conn) {
			connID := wonderProbe.registerTLSConnection(c.RemoteAddr(), c.LocalAddr())
			tc, ok := c.(*tls.Conn)
			if !ok {
				wonderProbe.record(probeEvent{
					Kind:         "tls_internal_error",
					ConnectionID: connID,
					Error:        "accepted connection is not *tls.Conn",
				})
				_ = c.Close()
				return
			}
			handshakeStarted := time.Now()
			handshakeTimeout := envDuration("NPLN_TLS_HANDSHAKE_TIMEOUT", 10*time.Second)
			_ = tc.SetDeadline(handshakeStarted.Add(handshakeTimeout))
			if err := tc.Handshake(); err != nil {
				log.Printf("[NPLN LOCAL] TLS Handshake failed from %s: %v", c.RemoteAddr(), err)
				wonderProbe.record(probeEvent{
					Kind:         "tls_handshake_failed",
					ConnectionID: connID,
					Error:        err.Error(),
					Fields: map[string]interface{}{
						"remote":      addressString(c.RemoteAddr()),
						"duration_ms": time.Since(handshakeStarted).Milliseconds(),
					},
				})
				wonderProbe.forgetConnection(addressString(c.RemoteAddr()))
				_ = c.Close()
				return
			}
			_ = tc.SetDeadline(time.Time{})
			state := tc.ConnectionState()
			proto := state.NegotiatedProtocol
			log.Printf("[NPLN LOCAL] TLS established: %s | SNI=%q | ALPN=%q | version=%s | cipher=%s",
				tc.RemoteAddr(), state.ServerName, proto, tls.VersionName(state.Version), tls.CipherSuiteName(state.CipherSuite))
			wonderProbe.record(probeEvent{
				Kind:         "tls_handshake_succeeded",
				ConnectionID: connID,
				Fields: map[string]interface{}{
					"remote":         addressString(tc.RemoteAddr()),
					"sni":            state.ServerName,
					"alpn":           proto,
					"tls_version":    tls.VersionName(state.Version),
					"cipher_suite":   tls.CipherSuiteName(state.CipherSuite),
					"resumed":        state.DidResume,
					"duration_ms":    time.Since(handshakeStarted).Milliseconds(),
					"grpc_candidate": proto == "h2" || proto == "grpc-exp",
				},
			})
			switch proto {
			case "h2", "grpc-exp":
				grpcLn.push(c)
			case "http/1.1", "":
				restLn.push(c)
			default:
				wonderProbe.record(probeEvent{
					Kind:         "tls_unsupported_alpn",
					ConnectionID: connID,
					Fields:       map[string]interface{}{"alpn": proto},
				})
				wonderProbe.forgetConnection(addressString(c.RemoteAddr()))
				_ = c.Close()
			}
		}(c)
	}
}
